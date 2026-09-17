"""S1c native tool probes, not a workload launcher or composed runtime image."""
import hashlib
import json
from pathlib import Path
import shlex
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path[:0] = [str(ROOT / "recovery/tooling"), str(Path(__file__).resolve().parents[1])]
from lab.host import LABEL, Lab

BASE_ID = "sha256:3f7a9a38c6ae0a779eb563ceb98cd70db6ae62243bf19ffbaa35587fa15ab807"
TESTS = (
    "src/app/fake.test.ts", "src/app/serve.test.ts", "src/app/shutdown.test.ts",
    "src/protocol/websocket/codec.test.ts", "src/protocol/websocket/limits.test.ts",
    "src/runtime/turn/fake.test.ts", "src/transport/http/handler.test.ts",
    "src/transport/http/local-access.test.ts", "src/transport/websocket/controls.test.ts",
    "src/transport/websocket/session.test.ts", "src/transport/websocket/writer.test.ts",
    "test/fixtures.ts", "test/loopback.smoke.ts", "test/shutdown-regression.smoke.ts",
)
ENVS = ["HOME=/home/claude", "USER=claude", "LOGNAME=claude", "PATH=/usr/bin:/bin",
        "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "CLAUDE_CONFIG_DIR=/home/claude/.claude",
        "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
        "BUN_INSTALL_CACHE_DIR=/home/claude/.bun-cache"]
TMPFS = {"/tmp": "rw,noexec,nosuid,nodev,size=64m,mode=1777",
         "/home/claude": "rw,noexec,nosuid,nodev,size=128m,mode=0700,uid=1000,gid=1000",
         "/opt/isthmus-probe": "rw,exec,nosuid,nodev,size=512m,mode=0755,uid=0,gid=0"}


def probe_args(name):
    args = ["create", "--name", name + "-toolchain", "--label", LABEL + "=" + name,
            "--network", "none", "--read-only", "--user", "1000:1000", "--cap-drop", "ALL",
            "--security-opt", "no-new-privileges", "--memory", "2g", "--memory-swap", "2g",
            "--cpus", "1", "--pids-limit", "128", "--ulimit", "core=0:0",
            "--ipc", "private", "--cgroupns", "private", "--restart", "no",
            "--log-driver", "local", "--log-opt", "max-size=5m", "--log-opt", "max-file=2",
            "--workdir", "/", "--entrypoint", "/usr/bin/timeout"]
    for key, value in TMPFS.items():
        args += ["--tmpfs", key + ":" + value]
    for value in ENVS:
        args += ["--env", value]
    return args + [BASE_ID, "--kill-after=5", "240", "/bin/sleep", "240"]


def validate_probe(value, name):
    h, c = value["HostConfig"], value["Config"]
    if (value["Image"] != BASE_ID or c["User"] != "1000:1000"
            or c.get("Labels", {}).get(LABEL) != name or sorted(c["Env"]) != sorted(ENVS)
            or c["WorkingDir"] != "/"
            or c["Entrypoint"] != ["/usr/bin/timeout"]
            or c["Cmd"] != ["--kill-after=5", "240", "/bin/sleep", "240"]):
        raise ValueError("toolchain_probe_identity_mismatch")
    if (h["Privileged"] or not h["ReadonlyRootfs"] or h["NetworkMode"] != "none"
            or h["Memory"] != 2147483648 or h["MemorySwap"] != 2147483648
            or h["NanoCpus"] != 1000000000 or h["PidsLimit"] != 128
            or h.get("CapAdd") or h["CapDrop"] != ["ALL"]
            or h["SecurityOpt"] != ["no-new-privileges"] or h.get("Binds")
            or h.get("PortBindings") or h.get("Devices") or h.get("DeviceRequests")
            or h.get("PidMode") not in ("", "private") or h["IpcMode"] != "private"
            or h["CgroupnsMode"] != "private" or h["RestartPolicy"]["Name"] != "no"
            or h["Tmpfs"] != TMPFS
            or h["Ulimits"] != [{"Name": "core", "Hard": 0, "Soft": 0}]
            or any(m["Type"] != "tmpfs" for m in value["Mounts"])):
        raise ValueError("toolchain_probe_isolation_mismatch")


def checksums(root):
    """Check the exact private payload inventory immediately before copy."""
    records = json.loads((root / "probe-payload.json").read_text())
    from lab.payload import expected_records
    if records != expected_records():
        raise ValueError("probe_unreviewed_payload")
    actual = []
    for path in sorted((root / "payload").rglob("*")):
        if path.is_symlink():
            raise ValueError("probe_symlink_rejected")
        if path.is_file():
            rel = path.relative_to(root / "payload").as_posix()
            expected = next((item for item in records if item["path"] == rel), None)
            if expected is None or path.stat().st_size != expected["size"]:
                raise ValueError("probe_unexpected_file_or_size")
            if path.stat().st_nlink != 1:
                raise ValueError("probe_hardlink_rejected")
            digest = hashlib.sha256()
            with path.open("rb") as source:
                total = 0
                while True:
                    chunk = source.read(min(1024 * 1024, expected["size"] + 1 - total))
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > expected["size"]:
                        raise ValueError("probe_payload_size_exceeded")
                    digest.update(chunk)
            actual.append({"path": rel, "size": path.stat().st_size, "sha256": digest.hexdigest()})
        elif not path.is_dir():
            raise ValueError("probe_special_file_rejected")
    if actual != records:
        raise ValueError("probe_payload_mismatch")
    return records


def script_for(records):
    # Paths are validated against our separately frozen source/test allowlist by
    # prepare; quote again here because this text becomes a shell command.
    lines = ["set -eu", "test \"$(id -u):$(id -g)\" = 1000:1000",
        "test \"$(ls /sys/class/net)\" = lo", "test \"$(ulimit -c)\" = 0",
        "test \"$(cat /sys/fs/cgroup/memory.max)\" = 2147483648",
        "test \"$(cat /sys/fs/cgroup/memory.swap.max)\" = 0",
        "test \"$(cat /sys/fs/cgroup/cpu.max)\" = '100000 100000'",
        "test \"$(cat /sys/fs/cgroup/pids.max)\" = 128",
        "awk '/^CapEff:/ {if ($2 != \"0000000000000000\") exit 1; cap=1} /^NoNewPrivs:/ {if ($2 != 1) exit 1; nnp=1} END {if (!cap || !nnp) exit 1}' /proc/self/status",
        "test -z \"$(find /home/claude -mindepth 1 -print -quit)\"",
        "test \"$(stat -c '%u:%g:%a' /home/claude)\" = 1000:1000:700",
        "test \"$(stat -c '%u:%g:%a' /opt/isthmus-probe)\" = 0:0:755",
        "test -z \"$(find /opt/isthmus-probe -writable -print -quit)\"",
        "awk '$2==\"/\" {n++; if ($4 !~ /(^|,)ro(,|$)/) exit 1} END {if (n != 1) exit 1}' /proc/mounts",
        "awk '$2==\"/opt/isthmus-probe\" {n++; if ($3 != \"tmpfs\" || $4 !~ /(^|,)rw(,|$)/ || $4 ~ /(^|,)noexec(,|$)/ || $4 !~ /(^|,)nosuid(,|$)/ || $4 !~ /(^|,)nodev(,|$)/) exit 1} END {if (n != 1) exit 1}' /proc/mounts",
        "awk '$2==\"/home/claude\" || $2==\"/tmp\" {n++; if ($3 != \"tmpfs\" || $4 !~ /(^|,)noexec(,|$)/) exit 1} END {if (n != 2) exit 1}' /proc/mounts",
        "if touch /etc/isthmus-s1c-write-negative 2>/dev/null; then exit 1; fi",
        "cd /opt/isthmus-probe"]
    for item in records:
        path = item["path"]
        if (not (path.startswith("app/src/") or path.startswith("app/test/")
                 or path in ("app/package.json", "app/contracts/grpc/messages.proto",
                             "bin/bun-1.4.2", "bin/bun-1.3.9", "bin/claude"))
                or any(part in ("", ".", "..") for part in path.split("/"))):
            raise ValueError("probe_path_rejected")
        lines.append("test \"$(sha256sum " + shlex.quote(path) + " | cut -d ' ' -f 1)\" = " + shlex.quote(item["sha256"]))
    lines += ["echo toolchain-isolation-and-hashes-pass", "cd app",
        "test \"$(../bin/claude --version)\" = '2.1.258 (Claude Code)'",
        "echo claude-native-version-pass", "test \"$(../bin/bun-1.4.2 --version)\" = 1.4.2",
        "test \"$(../bin/bun-1.3.9 --version)\" = 1.3.9",
        "../bin/bun-1.4.2 test", "../bin/bun-1.3.9 test",
        "i=0; while test $i -lt 5; do ../bin/bun-1.4.2 run test/loopback.smoke.ts; ../bin/bun-1.4.2 run test/shutdown-regression.smoke.ts; i=$((i+1)); done",
        "../bin/bun-1.3.9 run test/loopback.smoke.ts",
        "set +e", "../bin/bun-1.3.9 run test/shutdown-regression.smoke.ts > /tmp/control-shutdown.log 2>&1", "control=$?", "set -e",
        "cat /tmp/control-shutdown.log", "test \"$control\" = 1",
        "grep -Fx 'shutdown gate FAIL: server-initiated close/stop could not be verified; see runtime README' /tmp/control-shutdown.log",
        "test \"$(wc -l < /tmp/control-shutdown.log)\" = 1",
        "echo bun-1.3.9-expected-shutdown-gate-failure-reproduced",
        "echo native-toolchain-probe-pass"]
    return "\n".join(lines)


def probe(lab):
    if "avx2" not in Path("/proc/cpuinfo").read_text():
        raise ValueError("bun_control_avx2_required")
    available = int(next(line.split()[1] for line in Path("/proc/meminfo").read_text().splitlines()
                         if line.startswith("MemAvailable:")))
    if available < 3 * 1024**2:
        raise ValueError("toolchain_host_memory_reserve")
    records = checksums(lab.root)
    script = script_for(records)
    baseline = lab.baseline()
    lab.save("toolchain-baseline.json", baseline)
    cid = lab.run(*probe_args(lab.name)).stdout.strip()
    try:
        lab.save("toolchain-container.json", {"id": cid})
        validate_probe(lab.owned(cid), lab.name)
        lab.run("start", cid)
        from lab.payload import upload_payload
        upload_payload(lab, cid)
        validate_probe(lab.owned(cid), lab.name)
        lab.logged("toolchain-probe", ["exec", cid, "/usr/bin/timeout", "--kill-after=5", "120",
                                      "/bin/sh", "-ec", script], timeout=150, stop_on_abort=cid)
        value = lab.owned(cid)
        if value["State"]["OOMKilled"] or not value["State"]["Running"]:
            raise ValueError("native_probe_failed")
        log = (lab.root / "toolchain-probe.log").read_text()
        if not log.rstrip().endswith("native-toolchain-probe-pass"):
            raise ValueError("native_probe_incomplete")
        result = {"native_amd64": True, "arm64_executed": False,
            "claude_version": "2.1.258", "candidate_bun": "1.4.2", "control_bun": "1.3.9",
            "loopback_repetitions": 5, "shutdown_repetitions": 5,
            "control_shutdown_expected_failure": True, "real_model_requests": 0,
            "immutable_runtime_image_built": False, "runtime_ready": False}
    finally:
        # A lost SSH client is additionally bounded by the container's timeout.
        value = lab.owned(cid)
        if value["State"]["Running"]:
            lab.run("stop", "--time", "5", cid)
        lab.owned(cid)
        lab.run("rm", cid)
        unchanged = lab.baseline() == baseline
        lab.save("toolchain-cleanup.json", {"removed_container_id": cid, "existing_containers_unchanged": unchanged})
        if not unchanged:
            raise ValueError("existing_container_baseline_changed")
    # A probe is not accepted until its owned container is gone and coexistence
    # is checked. Failed cleanup must never leave a success receipt.
    lab.save("toolchain-result.json", result)


if __name__ == "__main__":
    if len(sys.argv) != 3 or sys.argv[1] not in ("acquire", "payload", "probe"):
        raise SystemExit("usage: toolchain.py acquire|payload|probe /var/tmp/isthmus-s1b.<run>")
    lab = Lab(sys.argv[2])
    try:
        if sys.argv[1] == "acquire":
            from lab.acquire import acquire
            acquire(lab, Path(__file__).resolve().parents[1] / "locks/toolchain-linux-2026-09-17.json")
        elif sys.argv[1] == "payload":
            from lab.payload import prepare_payload
            prepare_payload(lab)
        else:
            probe(lab)
    except Exception:
        # Raw network/error metadata stays outside the chat; no URL query or env.
        raise SystemExit("toolchain_phase_failed; inspect this private lab directory")
