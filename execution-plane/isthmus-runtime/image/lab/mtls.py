"""S2b synthetic Go component tests in one network-none Linux probe.

Not cross-container mTLS, CLI bridging, enrollment or a production launcher.
Reuses the existing image/resource/ownership checks; no daemon configuration.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[4]
sys.path[:0] = [str(ROOT / "recovery/tooling"), str(Path(__file__).resolve().parents[1])]
from lab.cli import isolation_script
from lab.host import Lab, bounded_process, environment
from lab.identity import _archive, _cleanup, _object
from lab.payload import root_upload
from lab.toolchain import probe_args, validate_probe
from recoverykit.evidence.filesystem import file_descriptor

NAMES = ("identity.test", "identity-command.test", "worker.test")
RUNS = (".", ".", "^TestProcessMTLS")


def inputs(lab):
    with file_descriptor(lab.root, "mtls-binaries.json") as (fd, info):
        if info.st_nlink != 1 or not 0 < info.st_size <= 4096:
            raise ValueError("mtls_manifest_rejected")
        data = os.read(fd, 4097)
    records = json.loads(data, object_pairs_hook=_object)
    if not isinstance(records, list) or len(records) != len(NAMES):
        raise ValueError("mtls_manifest_rejected")
    entries = []
    for record, name in zip(records, NAMES):
        if (not isinstance(record, dict) or set(record) != {"name", "size", "sha256"}
                or record["name"] != name or type(record["size"]) is not int
                or not 0 < record["size"] <= 64 * 1024**2
                or not isinstance(record["sha256"], str)
                or not re.fullmatch(r"[a-f0-9]{64}", record["sha256"])):
            raise ValueError("mtls_binary_record_rejected")
        origin = lab.root / "mtls-bin" / name
        digest, total = hashlib.sha256(), 0
        with file_descriptor(origin.parent, name) as (fd, info):
            if info.st_nlink != 1 or info.st_size != record["size"]:
                raise ValueError("mtls_binary_size_rejected")
            header = os.pread(fd, 20, 0)
            if header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
                raise ValueError("mtls_binary_architecture_rejected")
            while chunk := os.read(fd, min(1024**2, record["size"] + 1 - total)):
                digest.update(chunk)
                total += len(chunk)
                if total > record["size"]:
                    raise ValueError("mtls_binary_size_changed")
        if total != record["size"] or digest.hexdigest() != record["sha256"]:
            raise ValueError("mtls_binary_hash_mismatch")
        entries.append((origin, "bin/" + name, record, 0o755))
    return entries


def probe(lab):
    available = int(next(line.split()[1] for line in Path("/proc/meminfo").read_text().splitlines()
                         if line.startswith("MemAvailable:")))
    if available < 2 * 1024**2:
        raise ValueError("mtls_host_memory_reserve")
    entries = inputs(lab)
    archive = lab.root / "mtls-inputs.tar"
    _archive(archive, entries)
    baseline = lab.baseline()
    lab.save("mtls-baseline.json", baseline)
    containers, unresolved = {}, False
    try:
        unresolved = True
        cid = lab.run(*probe_args(lab.name, memory_gib=1)).stdout.strip()
        if not re.fullmatch(r"[a-f0-9]{64}", cid):
            raise ValueError("mtls_container_id_rejected")
        containers["mtls"] = cid
        unresolved = False
        lab.save("mtls-container.json", {"id": cid})
        validate_probe(lab.owned(cid), lab.name, memory_gib=1)
        lab.run("start", cid)
        with archive.open("rb") as source:
            uploaded = bounded_process(lab.prefix + ["exec", "--user", "0:0", "-i", cid,
                "/bin/sh", "-ec", root_upload(memory_bytes=1024**3)], environment(lab.config),
                60, 1024**2, check_budget=lab.capacity, input_fd=source.fileno())
        if uploaded.returncode or uploaded.stdout != "root-upload-capless-pass\n":
            raise ValueError("mtls_upload_failed")
        script = isolation_script(memory_bytes=1024**3)
        for _, name, record, _ in entries:
            script += 'test "$(sha256sum ' + name + " | cut -d ' ' -f 1)\" = " + record["sha256"] + "\n"
        for name, run in zip(NAMES, RUNS):
            script += "bin/" + name + " -test.v -test.count=3 -test.timeout=30s -test.run='" + run + "'\n"
        script += "echo synthetic-mtls-native-pass\n"
        lab.logged("mtls-native", ["exec", "--user", "1000:1000", cid, "/usr/bin/timeout", "--kill-after=5", "130",
            "/bin/sh", "-ec", script], timeout=145, stop_on_abort=cid)
        value = lab.owned(cid)
        validate_probe(value, lab.name, memory_gib=1)
        if value["State"]["OOMKilled"] or not value["State"]["Running"]:
            raise ValueError("mtls_probe_container_failed")
        if not (lab.root / "mtls-native.log").read_text().rstrip().endswith("synthetic-mtls-native-pass"):
            raise ValueError("mtls_tests_incomplete")
    finally:
        _cleanup(lab, containers, baseline, unresolved_create=unresolved)
    lab.save("mtls-result.json", {"native_component_tests": True, "repetitions": 3,
        "actual_worker_and_controller_tls": True, "synthetic_credentials_only": True,
        "real_model_requests": 0, "cross_container_mtls": False, "authenticated_enrollment": False,
        "cli_bridge": False, "restricted_egress_verified": False, "production_ready": False})


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("explicit private lab root required")
    try:
        probe(Lab(sys.argv[1]))
    except Exception:
        raise SystemExit("mtls_probe_failed; inspect private bounded evidence")
