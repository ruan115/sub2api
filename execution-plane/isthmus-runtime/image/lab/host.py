"""Explicit trusted-package lab helpers; never a workload isolation permit.

No implicit Docker context/config/environment; no automatic cleanup or deployment.
Only run on the reviewed test daemon. Public preparation/build logs stay private.
"""
import json
import os
from pathlib import Path
import re
import selectors
import shutil
import subprocess
import time

ENGINE_ID = "18f7810b-1ae6-4a3a-8825-a9d38136afd9"
BASE = "docker.io/library/debian@sha256:abc9cb88a5587630d7f915f47b23b0668fe250fbfc6457aa4d52b534c1bbf73f"
BUILDKIT = "docker.io/moby/buildkit@sha256:a461e7f0ce921972028acfbed628d45663d83e67ac1230722c2b34cf72760a0d"
LABEL = "org.sub2api.isthmus.lab-run"


def bounded_process(argv, env, timeout, limit, sink=None, check_budget=None, input_fd=None):
    process = subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL if input_fd is None else input_fd,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    result, total, start = {"stdout": [], "stderr": []}, 0, time.monotonic()
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ, "stdout")
            selector.register(process.stderr, selectors.EVENT_READ, "stderr")
            while selector.get_map() or process.poll() is None:
                if time.monotonic() - start > timeout:
                    raise TimeoutError("lab_command_deadline")
                if check_budget is not None:
                    check_budget()
                for key, _ in selector.select(0.1):
                    data = os.read(key.fileobj.fileno(), min(65536, limit + 1 - total))
                    if not data:
                        selector.unregister(key.fileobj)
                        continue
                    total += len(data)
                    if total > limit:
                        raise ValueError("docker_output_limit")
                    if sink is not None:
                        sink.write(data)
                    else:
                        result[key.data].append(data)
            if check_budget is not None:
                check_budget()
            if time.monotonic() - start > timeout:
                raise TimeoutError("lab_command_deadline")
            return subprocess.CompletedProcess(argv, process.wait(),
                b"".join(result["stdout"]).decode("utf-8"),
                b"".join(result["stderr"]).decode("utf-8"))
    finally:
        if process.poll() is None:
            process.kill()
        process.wait()
        process.stdout.close()
        process.stderr.close()


def environment(config):
    return {"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
            "DOCKER_CONFIG": str(config), "BUILDX_NO_DEFAULT_ATTESTATIONS": "1"}


def validate_builder(value, *, image_id, volume, owner, command):
    h = value["HostConfig"]
    if not (h["Memory"] == 2147483648 and h["MemorySwap"] == 2147483648
            and h["CpuQuota"] == 200000 and h["CpuPeriod"] == 100000
            and h["RestartPolicy"]["Name"] == "no" and h["Privileged"] is True
            and h["PidsLimit"] == 512
            and not h.get("Binds") and not h.get("PortBindings")
            and h["NetworkMode"] == "bridge"):
        raise ValueError("builder_limits_or_scope_mismatch")
    mounts = value["Mounts"]
    if (len(mounts) != 1 or mounts[0]["Type"] != "volume"
            or mounts[0]["Destination"] != "/var/lib/buildkit" or mounts[0]["Name"] != volume):
        raise ValueError("builder_mount_mismatch")
    if (value["Image"] != image_id or value["Config"].get("Labels", {}).get(LABEL) != owner
            or value["Config"]["Cmd"] != command or value["Config"]["Entrypoint"] != ["/bin/sh"]
            or h.get("PidMode") not in ("", "private")
            or h.get("IpcMode") != "private" or h.get("CgroupnsMode") != "private"):
        raise ValueError("builder_identity_mismatch")
    if any(item.split("=", 1)[0].lower().endswith("proxy") for item in value["Config"]["Env"]):
        raise ValueError("builder_proxy_injected")


class Lab:
    def __init__(self, root):
        self.root = Path(root)
        if (not self.root.is_absolute() or self.root.resolve() != self.root
                or self.root.parent != Path("/var/tmp")
                or not re.fullmatch(r"isthmus-s1b\.[A-Za-z0-9_-]{6,32}", self.root.name)
                or self.root.stat().st_uid != os.getuid()
                or self.root.stat().st_mode & 0o777 != 0o700):
            raise ValueError("private_lab_root_required")
        self.name = self.root.name.replace(".", "-").lower()
        self.config = self.root / "docker-config"
        self.config.mkdir(mode=0o700, exist_ok=True)
        if self.config.is_symlink() or self.config.stat().st_mode & 0o777 != 0o700:
            raise ValueError("private_docker_config_required")
        # Our CLI never writes config.json, auths, proxy or credential helpers.
        if (self.config / "config.json").exists():
            raise ValueError("docker_client_config_rejected")
        self.prefix = ["/usr/bin/docker", "--config", str(self.config),
                       "--host", "unix:///var/run/docker.sock"]
        info = self.json("info", "--format", "{{json .}}")
        if (info["ID"] != ENGINE_ID or info["Name"] != "VM-0-12-ubuntu"
                or info["OSType"] != "linux" or info["Architecture"] != "x86_64"
                or info["CgroupVersion"] != "2" or info["CgroupDriver"] != "systemd" or info["NCPU"] != 4
                or any(info.get(k) for k in ("HttpProxy", "HttpsProxy", "NoProxy"))):
            raise ValueError("wrong_daemon_or_proxy")
        if not (self.root / "disk-baseline.json").exists():
            self.save("disk-baseline.json", {"free_bytes": shutil.disk_usage(self.root).free})
        self.capacity()

    def capacity(self):
        free = shutil.disk_usage(self.root).free
        initial = json.loads((self.root / "disk-baseline.json").read_text())["free_bytes"]
        if free < 20 * 1024**3 or initial - free > 8 * 1024**3:
            raise ValueError("disk_reserve_exhausted")

    def run(self, *args, timeout=30, check=True):
        result = bounded_process(self.prefix + list(args), environment(self.config),
                                 timeout, 4 * 1024**2)
        if check and result.returncode:
            raise RuntimeError("docker_command_failed:" + args[0])
        return result

    def json(self, *args):
        return json.loads(self.run(*args).stdout)

    def save(self, name, value):
        if not re.fullmatch(r"[a-z0-9_.-]+\.json", name):
            raise ValueError("invalid_evidence_name")
        with (self.root / name).open("x", encoding="utf-8") as out:
            os.chmod(out.fileno(), 0o600)
            json.dump(value, out, sort_keys=True, indent=2)
            out.write("\n")

    def logged(self, name, args, *, timeout=900, stop_on_abort=None, observer=None):
        if not re.fullmatch(r"[a-z0-9_.-]+", name):
            raise ValueError("invalid_log_name")
        with (self.root / (name + ".log")).open("xb") as out:
            os.chmod(out.fileno(), 0o600)
            start = time.monotonic()
            try:
                def budget():
                    self.capacity()
                    if observer is not None:
                        observer()
                code = bounded_process(self.prefix + args, environment(self.config), timeout,
                                       16 * 1024**2, out, self.capacity if observer is None else budget).returncode
            except Exception:
                if stop_on_abort is not None:
                    self.owned(stop_on_abort)
                    self.run("stop", "--time", "5", stop_on_abort)
                raise
            self.save(name + ".json", {"exit_code": code,
                                      "elapsed_seconds": round(time.monotonic() - start, 2)})
            if code:
                raise RuntimeError("logged_command_failed:" + name)

    def baseline(self):
        ids = self.run("ps", "-aq", "--no-trunc").stdout.split()
        # Project inside the CLI; never capture existing business Env/Cmd/logs.
        template = ('{"Id":{{json .Id}},"Name":{{json .Name}},"Image":{{json .Image}},'
                    '"RestartCount":{{json .RestartCount}},"State":{"StartedAt":{{json .State.StartedAt}},'
                    '"Status":{{json .State.Status}}},"Mounts":{{json .Mounts}}}')
        values = [json.loads(line) for line in self.run("inspect", "--format", template, *ids).stdout.splitlines()] if ids else []
        return sorted([{"id": v["Id"], "name": v["Name"], "image": v["Image"],
                        "started_at": v["State"]["StartedAt"], "status": v["State"]["Status"],
                        "restart_count": v["RestartCount"],
                        "mounts": [{k: m.get(k) for k in ("Type", "Name", "Destination", "RW")}
                                   for m in v["Mounts"]]}
                       for v in values if not v["Name"].lstrip("/").startswith(self.name)
                       and v["Name"] != "/buildx_buildkit_" + self.name + "0"],
                      key=lambda item: item["id"])

    def owned(self, container_id):
        if not re.fullmatch(r"[a-f0-9]{64}", container_id):
            raise ValueError("exact_container_id_required")
        value = self.json("inspect", container_id)[0]
        if value["Config"].get("Labels", {}).get(LABEL) != self.name:
            raise ValueError("container_ownership_mismatch")
        return value

    def create_probe(self, suffix, image, command, *, network="none", memory="128m"):
        if suffix not in ("prepare", "smoke", "default", "negative") or network not in ("none", "bridge"):
            raise ValueError("probe_scope_rejected")
        args = ["create", "--name", self.name + "-" + suffix, "--label", LABEL + "=" + self.name,
                "--network", network, "--memory", memory, "--memory-swap", memory,
                "--cpus", "0.5", "--pids-limit", "128" if suffix == "prepare" else "64", "--security-opt", "no-new-privileges",
                "--cap-drop", "ALL", "--log-driver", "local", "--log-opt", "max-size=5m",
                "--log-opt", "max-file=2"]
        if suffix != "prepare":
            args += ["--read-only", "--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"]
        else:
            # APT drops to its _apt account to verify/fetch official indexes;
            # it must be able to change identity and own its private cache.
            args += ["--cap-add", "CHOWN", "--cap-add", "SETUID", "--cap-add", "SETGID",
                     "--cap-add", "FOWNER", "--cap-add", "DAC_OVERRIDE"]
        return self.run(*args, image, *command).stdout.strip()
