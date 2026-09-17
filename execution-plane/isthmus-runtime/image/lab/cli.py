"""Single-instance real CLI -> synthetic loopback. No downloads or real accounts."""
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tarfile

ROOT = Path(__file__).resolve().parents[4]
sys.path[:0] = [str(ROOT / "recovery/tooling"), str(Path(__file__).resolve().parents[1])]
from lab.host import Lab, bounded_process, environment
from lab.toolchain import probe_args, validate_probe
from lab.payload import ROOT_UPLOAD
from recoverykit.evidence.filesystem import file_descriptor

SOURCE_FILES = (
    "src/runtime/cli/config.ts", "src/runtime/cli/events.ts", "src/runtime/cli/process.ts",
    "src/runtime/turn/errors.ts", "src/transport/http/local-access.ts",
    "src/transport/shared/body.ts", "src/app/cli/serve.ts", "src/app/shutdown.ts",
    "test/cli/stub.ts", "test/cli/roundtrip.smoke.ts",
)
LOCK = Path(__file__).resolve().parents[1] / "locks/toolchain-linux-amd64-expanded-2026-09-17.json"


def inputs(lab):
    records = json.loads((lab.root / "cli-source.json").read_text())
    if [e["path"] for e in records] != list(SOURCE_FILES):
        raise ValueError("cli_source_allowlist_mismatch")
    if any(set(e) != {"path", "size", "sha256"} or not isinstance(e["sha256"], str)
           or not re.fullmatch(r"[a-f0-9]{64}", e["sha256"]) for e in records):
        raise ValueError("cli_source_digest_rejected")
    binaries = json.loads(LOCK.read_text())["binaries"]
    selected = [e for e in binaries if e["file"] in ("bun-1.4.2", "claude")]
    if len(selected) != 2:
        raise ValueError("cli_tool_lock_mismatch")
    entries = [(ROOT / "execution-plane/isthmus-runtime" / e["path"], "app/" + e["path"], e, 0o644) for e in records]
    entries += [(lab.root / "toolchain-bin" / e["file"], "bin/" + e["file"], e, 0o755) for e in selected]
    for origin, name, record, mode in entries:
        size = record["size"]
        if type(size) is not int or not 0 < size <= (256 * 1024**2 if mode == 0o755 else 128 * 1024):
            raise ValueError("cli_input_size_rejected")
        digest = hashlib.sha256()
        with file_descriptor(origin.parent, origin.name) as (fd, info):
            if info.st_nlink != 1 or info.st_size != size:
                raise ValueError("cli_input_type_rejected")
            total = 0
            while True:
                chunk = os.read(fd, min(1024 * 1024, size + 1 - total))
                if not chunk:
                    break
                total += len(chunk)
                if total > size:
                    raise ValueError("cli_input_size_changed")
                digest.update(chunk)
        if total != size or digest.hexdigest() != record["sha256"]:
            raise ValueError("cli_input_hash_mismatch")
    return entries


def probe_script(entries, *, require_empty_home=True, smoke=True, memory_bytes=2147483648):
    """Shared UID1000 isolation/hash gates; identity probes retain a nonempty home."""
    if type(memory_bytes) is not int or memory_bytes not in (1024**3, 2 * 1024**3):
        raise ValueError("cli_probe_memory_rejected")
    script = """set -eu
test "$(id -u):$(id -g)" = 1000:1000
test "$(ls /sys/class/net)" = lo
test "$(ulimit -c)" = 0
test "$(cat /sys/fs/cgroup/memory.max)" = MEMORY_BYTES
test "$(cat /sys/fs/cgroup/memory.swap.max)" = 0
test "$(cat /sys/fs/cgroup/cpu.max)" = '100000 100000'
test "$(cat /sys/fs/cgroup/pids.max)" = 128
awk '/^CapEff:/ {if ($2 != "0000000000000000") exit 1; cap=1} /^NoNewPrivs:/ {if ($2 != 1) exit 1; nnp=1} END {if (!cap || !nnp) exit 1}' /proc/self/status
test "$(stat -c '%u:%g:%a' /home/claude)" = 1000:1000:700
test -z "$(find /opt/isthmus-probe -writable -print -quit)"
awk '$2=="/" {n++; if ($4 !~ /(^|,)ro(,|$)/) exit 1} END {if (n != 1) exit 1}' /proc/mounts
awk '$2=="/home/claude" || $2=="/tmp" {n++; if ($3 != "tmpfs" || $4 !~ /(^|,)noexec(,|$)/) exit 1} END {if (n != 2) exit 1}' /proc/mounts
cd /opt/isthmus-probe
""".replace("MEMORY_BYTES", str(memory_bytes))
    if require_empty_home:
        script += 'test -z "$(find /home/claude -mindepth 1 -print -quit)"\n'
    # Paths come solely from SOURCE_FILES and the checked-in binary lock (or
    # the identity harness's one explicitly checked helper binary).
    allowed = {"app/" + path for path in SOURCE_FILES} | {"bin/claude", "bin/bun-1.4.2", "bin/instance-identity"}
    for _, name, record, _ in entries:
        if name not in allowed or not isinstance(record.get("sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", record["sha256"]):
            raise ValueError("cli_script_input_rejected")
        script += "test \"$(sha256sum " + name + " | cut -d ' ' -f 1)\" = " + record["sha256"] + "\n"
    script += """test "$(bin/claude --version)" = '2.1.258 (Claude Code)'
test "$(bin/bun-1.4.2 --version)" = 1.4.2
echo cli-image-inputs-and-isolation-pass
"""
    if smoke:
        script += "exec bin/bun-1.4.2 app/test/cli/roundtrip.smoke.ts\n"
    return script


def probe(lab):
    if "avx2" not in Path("/proc/cpuinfo").read_text():
        raise ValueError("cli_host_cpu_mismatch")
    available = int(next(line.split()[1] for line in Path("/proc/meminfo").read_text().splitlines()
                         if line.startswith("MemAvailable:")))
    if available < 3 * 1024**2:
        raise ValueError("cli_host_memory_reserve")
    entries = inputs(lab)
    archive = lab.root / "cli-inputs.tar"
    with archive.open("xb") as output:
        os.chmod(output.fileno(), 0o600)
        with tarfile.open(fileobj=output, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for origin, name, record, mode in entries:
                with file_descriptor(origin.parent, origin.name) as (fd, info):
                    if info.st_nlink != 1 or info.st_size != record["size"]:
                        raise ValueError("cli_input_changed")
                    header = tarfile.TarInfo(name)
                    header.size, header.mode = info.st_size, mode
                    header.uid = header.gid = header.mtime = 0
                    with os.fdopen(os.dup(fd), "rb") as source:
                        tar.addfile(header, source)
    baseline = lab.baseline()
    lab.save("cli-baseline.json", baseline)
    cid = lab.run(*probe_args(lab.name)).stdout.strip()
    try:
        lab.save("cli-container.json", {"id": cid})
        validate_probe(lab.owned(cid), lab.name)
        lab.run("start", cid)
        with archive.open("rb") as source:
            result = bounded_process(lab.prefix + ["exec", "--user", "0:0", "-i", cid,
                "/bin/sh", "-ec", ROOT_UPLOAD], environment(lab.config), 90, 1024 * 1024,
                check_budget=lab.capacity, input_fd=source.fileno())
        if result.returncode or result.stdout != "root-upload-capless-pass\n":
            raise ValueError("cli_upload_failed")
        validate_probe(lab.owned(cid), lab.name)
        script = probe_script(entries)
        lab.logged("cli-roundtrip", ["exec", cid, "/usr/bin/timeout", "--kill-after=5", "150",
                                   "/bin/sh", "-ec", script], timeout=170, stop_on_abort=cid)
        value = lab.owned(cid)
        if value["State"]["OOMKilled"] or not value["State"]["Running"]:
            raise ValueError("cli_probe_container_failed")
        if not (lab.root / "cli-roundtrip.log").read_text().rstrip().endswith(
                "cli-roundtrip smoke PASS: synthetic only, real_model_requests=0, owned listeners stopped"):
            raise ValueError("cli_probe_incomplete")
    finally:
        value = lab.owned(cid)
        if value["State"]["Running"]:
            lab.run("stop", "--time", "5", cid)
        lab.owned(cid)
        lab.run("rm", cid)
        unchanged = lab.baseline() == baseline
        lab.save("cli-cleanup.json", {"removed_container_id": cid, "existing_containers_unchanged": unchanged})
        if not unchanged:
            raise ValueError("existing_container_baseline_changed")
    lab.save("cli-result.json", {"single_instance_roundtrip": True, "real_cli": True,
        "real_model_requests": 0, "synthetic_usage": True, "production_ready": False})


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("explicit private lab root required")
    try:
        probe(Lab(sys.argv[1]))
    except Exception:
        raise SystemExit("cli_probe_failed; inspect private bounded evidence")
