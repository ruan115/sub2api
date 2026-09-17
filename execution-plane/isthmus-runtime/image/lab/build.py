"""One pinned trusted base build; privileged builder is not a workload sandbox."""
import hashlib
import json
from pathlib import Path
import time

from imagekit.context import verify_context
from lab.host import BUILDKIT, LABEL, validate_builder


def read(lab, name):
    return json.loads((lab.root / name).read_text())


class CgroupObserver:
    def __init__(self, lab, cid):
        self.lab, self.cid, self.last = lab, cid, 0
        self.commands, self.groups = set(), set()
        pid = lab.owned(cid)["State"]["Pid"]
        self.parent = self.limit_root(cid)
        main_group = self.group(pid)
        if main_group != self.parent and not main_group.startswith(self.parent + "/"):
            raise ValueError("unbounded_builder_cgroup")

    @staticmethod
    def limit_root(cid):
        # The approved daemon uses systemd and no custom cgroup-parent. BuildKit
        # moves its main process into /init; that child is NOT the Docker budget
        # boundary. Bind to this exact container's scope and check real limits.
        import re
        if not re.fullmatch(r"[a-f0-9]{64}", cid):
            raise ValueError("exact_container_id_required")
        parent = "/system.slice/docker-" + cid + ".scope"
        root = Path("/sys/fs/cgroup" + parent)
        for name, expected in (("memory.max", "2147483648"), ("memory.swap.max", "0"),
                               ("cpu.max", "200000 100000"), ("pids.max", "512")):
            if (root / name).read_text().strip() != expected:
                raise ValueError("builder_host_cgroup_limit_mismatch")
        return parent

    @staticmethod
    def group(pid):
        text = Path("/proc") / str(pid) / "cgroup"
        values = [line[3:] for line in text.read_text().splitlines() if line.startswith("0::")]
        if len(values) != 1:
            raise ValueError("unified_cgroup_required")
        return values[0]

    def __call__(self):
        if time.monotonic() - self.last < 0.2:
            return
        self.last = time.monotonic()
        output = self.lab.run("top", self.cid, "-eo", "pid,comm").stdout
        for line in output.splitlines()[1:]:
            pid, command = line.split(None, 1)
            try:
                group = self.group(int(pid))
            except FileNotFoundError:
                continue  # An observed child may exit before /proc is read.
            if group != self.parent and not group.startswith(self.parent + "/"):
                self.lab.save("cgroup-rejected.json", {"parent": self.parent, "observed": group,
                                                       "pid": int(pid), "command": command})
                raise ValueError("build_child_escaped_cgroup")
            self.commands.add(command)
            self.groups.add(group)


def build(lab):
    verify_context(lab.root / "context")
    name, volume = lab.name + "-builder", lab.name + "-buildkit-cache"
    if lab.run("volume", "ls", "--filter", "name=^" + volume + "$", "--format", "{{.Name}}").stdout.strip():
        raise ValueError("builder_volume_already_exists")
    lab.run("volume", "create", "--label", LABEL + "=" + lab.name, volume)
    lab.save("builder-volume.json", {"name": volume})
    command = ["-ec", "exec timeout -s TERM -k 5 900 buildkitd --config /tmp/isthmus-buildkitd.toml --addr unix:///run/buildkit/buildkitd.sock"]
    cid = lab.run("create", "--name", name, "--label", LABEL + "=" + lab.name,
        "--privileged", "--cgroupns", "private", "--ipc", "private", "--network", "bridge",
        "--memory", "2147483648", "--memory-swap", "2147483648", "--cpu-quota", "200000",
        "--cpu-period", "100000", "--pids-limit", "512", "--restart", "no",
        "--log-driver", "local", "--log-opt", "max-size=5m", "--log-opt", "max-file=2",
        "--mount", "type=volume,source=" + volume + ",target=/var/lib/buildkit",
        "--entrypoint", "/bin/sh", BUILDKIT, *command).stdout.strip()
    lab.save("builder-container.json", {"id": cid, "volume": volume})
    image_id = read(lab, "pull-builder-image.json")["Id"]
    validate_builder(lab.owned(cid), image_id=image_id, volume=volume, owner=lab.name, command=command)
    v = lab.json("volume", "inspect", volume)[0]
    if v.get("Labels", {}).get(LABEL) != lab.name:
        raise ValueError("builder_volume_ownership_mismatch")
    lab.run("cp", str(Path(__file__).parent / "buildkitd.toml"), cid + ":/tmp/isthmus-buildkitd.toml")
    lab.run("cp", str(lab.root / "context"), cid + ":/tmp/isthmus-context", timeout=60)
    try:
        lab.run("start", cid)
        cg = lab.run("exec", cid, "sh", "-ec",
                     "cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.swap.max /sys/fs/cgroup/cpu.max").stdout.splitlines()
        if cg != ["2147483648", "0", "200000 100000"]:
            raise ValueError("builder_cgroup_limit_mismatch")
        lab.save("builder-cgroup.json", {"memory_max": cg[0], "swap_max": cg[1], "cpu_max": cg[2]})
        for attempt in range(15):
            result = lab.run("exec", cid, "buildctl", "debug", "workers", check=False)
            if result.returncode == 0 and "linux/amd64" in result.stdout:
                break
            time.sleep(1)
        else:
            raise ValueError("builder_not_ready")
        lab.logged("copied-context-check", ["exec", cid, "sh", "-ec",
            "cd /tmp/isthmus-context/packages && sha256sum -c ../packages.sha256"])
        recipe_hash = lab.run("exec", cid, "sha256sum", "/tmp/isthmus-context/Dockerfile").stdout.split()[0]
        if recipe_hash != hashlib.sha256((lab.root / "context/Dockerfile").read_bytes()).hexdigest():
            raise ValueError("copied_recipe_mismatch")
        verify_context(lab.root / "context")
        tag = "isthmus-vm-base-lab:" + lab.name
        if lab.run("image", "ls", "--filter", "reference=" + tag, "--format", "{{.ID}}").stdout.strip():
            raise ValueError("output_tag_already_exists")
        request = ["exec", cid, "buildctl", "build", "--frontend", "dockerfile.v0",
                   "--local", "context=/tmp/isthmus-context", "--local", "dockerfile=/tmp/isthmus-context",
                   "--opt", "platform=linux/amd64", "--no-cache", "--progress", "plain"]
        observer = CgroupObserver(lab, cid)
        lab.logged("base-build", request + ["--output", "type=docker,name=" + tag + ",dest=/tmp/isthmus-base.tar"],
                   timeout=850, stop_on_abort=cid, observer=observer)
        lab.save("build-child-cgroups.json", {"parent": observer.parent,
                 "groups": sorted(observer.groups), "commands": sorted(observer.commands),
                 "dpkg_observed": "dpkg" in observer.commands})
        lab.run("cp", cid + ":/tmp/isthmus-base.tar", str(lab.root / "base-image.tar"), timeout=60)
        lab.logged("base-load", ["image", "load", "--input", str(lab.root / "base-image.tar")], timeout=120)
        item = lab.json("image", "inspect", tag)[0]
        if item["Config"]["User"] != "1000:1000" or item["Architecture"] != "amd64" or item["Config"]["Cmd"] != ["/bin/false"]:
            raise ValueError("base_image_contract_mismatch")
        if (set(e.split("=", 1)[0] for e in item["Config"]["Env"]) != {"PATH", "HOME", "USER", "LOGNAME"}
                or item["Config"].get("Volumes") or item["Config"].get("ExposedPorts")):
            raise ValueError("base_image_injected_config")
        completed = {"id": item["Id"], "tag": tag, "environment": item["Config"]["Env"],
                     "architecture": item["Architecture"], "base_only": True}
        # Same reviewed context, invalid target: no arbitrary package execution.
        negative = request + ["--opt", "target=missing-s1b-negative",
                               "--output", "type=docker,dest=/tmp/isthmus-negative.tar"]
        try:
            lab.logged("build-negative", negative, timeout=60, stop_on_abort=cid)
        except RuntimeError:
            if read(lab, "build-negative.json")["exit_code"] == 0:
                raise
            if 'target stage "missing-s1b-negative" could not be found' not in (lab.root / "build-negative.log").read_text():
                raise ValueError("negative_build_wrong_failure")
        else:
            raise ValueError("negative_build_unexpected_success")
        # buildctl may create a zero-byte destination before solving. It is not
        # an image; any nonempty output on this failed solve is rejected.
        lab.run("exec", cid, "test", "!", "-s", "/tmp/isthmus-negative.tar")
    finally:
        lab.owned(cid)
        lab.run("stop", "--time", "5", cid)
    lab.save("built-image.json", completed)
    print(json.dumps({"phase": "build", "status": "base_only_image_built"}))
