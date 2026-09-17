"""Own two finite-lifetime workers; publish success only after exact cleanup."""
from pathlib import Path
import re
import time

from lab.identity import _cleanup
from lab.livebootstrap import inputs, policy
from lab.livebootstrap.driver import Driver


def kernel_script(digest):
    if not re.fullmatch(r"[a-f0-9]{64}", digest):
        raise ValueError("live_digest_rejected")
    return """set -eu
test "$(id -u):$(id -g)" = 1000:1000
test "$(ls /sys/class/net)" = lo
test "$(ulimit -c)" = 0
test "$(cat /sys/fs/cgroup/memory.max)" = 1073741824
test "$(cat /sys/fs/cgroup/memory.swap.max)" = 0
test "$(cat /sys/fs/cgroup/cpu.max)" = '100000 100000'
test "$(cat /sys/fs/cgroup/pids.max)" = 128
awk '/^CapEff:/ {if ($2 != "0000000000000000") exit 1; cap=1} /^NoNewPrivs:/ {if ($2 != 1) exit 1; nnp=1} END {if (!cap || !nnp) exit 1}' /proc/self/status
test "$(stat -c '%u:%g:%a' /home/claude)" = 1000:1000:700
test "$(stat -c '%u:%a' /worker)" = 0:555
test ! -w /worker
awk '$2=="/" || $2=="/worker" {n++; if ($4 !~ /(^|,)ro(,|$)/) exit 1} END {if (n != 2) exit 1}' /proc/mounts
awk '$2=="/home/claude" || $2=="/tmp" || $2=="/run" {n++; if ($3 != "tmpfs" || $4 !~ /(^|,)noexec(,|$)/) exit 1} END {if (n != 3) exit 1}' /proc/mounts
test "$(sha256sum /worker | cut -d ' ' -f 1)" = DIGEST
echo live-worker-kernel-pass
""".replace("DIGEST", digest)


def probe(lab):
    available = int(next(line.split()[1] for line in Path("/proc/meminfo").read_text().splitlines()
                         if line.startswith("MemAvailable:")))
    if available < 3 * 1024**2:
        raise ValueError("live_host_memory_reserve")
    records = inputs.verify(lab)
    baseline = lab.baseline()
    lab.save("live-baseline.json", baseline)
    containers, unresolved = {}, False
    started = time.monotonic()
    try:
        with Driver(lab) as driver:
            lab.save("live-public-config.json", driver.public)
            for label in ("a", "b"):
                inputs.verify(lab)
                unresolved = True
                cid = lab.run(*policy.args(lab, label, driver.public), timeout=8).stdout.strip()
                if not re.fullmatch(r"[a-f0-9]{64}", cid) or cid in containers.values():
                    raise ValueError("live_container_id_rejected")
                containers[label] = cid
                unresolved = False
                lab.save("live-container-" + label + ".json", {"id": cid})
                policy.validate(lab.owned(cid), lab, label, driver.public)
                lab.run("start", cid, timeout=8)
                state = lab.owned(cid)
                policy.validate(state, lab, label, driver.public)
                if not state["State"]["Running"] or state["State"]["OOMKilled"]:
                    raise ValueError("live_worker_start_failed")
                readback = lab.run("exec", "--user", "1000:1000", cid, "/bin/sh", "-ec",
                                   kernel_script(records[0]["sha256"]), timeout=8)
                if readback.stdout != "live-worker-kernel-pass\n":
                    raise ValueError("live_worker_kernel_rejected")
            result = driver.finish([containers["a"], containers["b"]])
            inputs.verify(lab)
            for label, cid in containers.items():
                state = lab.owned(cid)
                policy.validate(state, lab, label, driver.public)
                if not state["State"]["Running"] or state["State"]["OOMKilled"]:
                    raise ValueError("live_worker_completion_failed")
    finally:
        _cleanup(lab, containers, baseline, unresolved_create=unresolved, evidence_name="live-cleanup.json")
    result.update({"elapsed_seconds": round(time.monotonic() - started, 2), "real_model_requests": 0,
                   "kernel_resource_readback": True, "existing_containers_unchanged": True,
                   "production_provider_adoption": False, "cli_bridge": False,
                   "public_binary_bind_only": True, "private_keys_exported": False})
    lab.save("live-result.json", result)
