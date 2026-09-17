"""Two instance-local identity probes; no enrollment, TLS or real model access."""
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tarfile
import tempfile
import time

ROOT = Path(__file__).resolve().parents[4]
sys.path[:0] = [str(ROOT / "recovery/tooling"), str(Path(__file__).resolve().parents[1])]
from lab import cli
from lab.host import Lab, bounded_process, environment
from lab.payload import root_upload
from lab.toolchain import probe_args, validate_probe
from recoverykit.evidence.filesystem import file_descriptor

DIRECTORY = "/home/claude/.isthmus-identity"
BINARY = "/opt/isthmus-probe/bin/instance-identity"
CLI_PASS = "cli-roundtrip smoke PASS: synthetic only, real_model_requests=0, owned listeners stopped"
DENIED = "instance identity operation rejected\n"


def _object(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("identity_duplicate_json_key")
        value[key] = item
    return value


def inputs(lab):
    entries = cli.inputs(lab)
    with file_descriptor(lab.root, "identity-binary.json") as (fd, info):
        if info.st_nlink != 1 or not 0 < info.st_size <= 4096:
            raise ValueError("identity_binary_record_rejected")
        raw = os.read(fd, 4097)
    record = json.loads(raw, object_pairs_hook=_object)
    if (not isinstance(record, dict) or set(record) != {"size", "sha256"}
            or type(record["size"]) is not int or not 0 < record["size"] <= 16 * 1024**2
            or not isinstance(record["sha256"], str) or not re.fullmatch(r"[a-f0-9]{64}", record["sha256"])):
        raise ValueError("identity_binary_record_rejected")
    origin = lab.root / "identity-bin/instance-identity"
    digest = hashlib.sha256()
    total = 0
    with file_descriptor(origin.parent, origin.name) as (fd, info):
        if info.st_nlink != 1 or info.st_size != record["size"]:
            raise ValueError("identity_binary_size_rejected")
        header = os.pread(fd, 20, 0)
        if len(header) != 20 or header[:6] != b"\x7fELF\x02\x01" or header[18:20] != b"\x3e\x00":
            raise ValueError("identity_binary_architecture_rejected")
        while True:
            chunk = os.read(fd, min(1024 * 1024, record["size"] + 1 - total))
            if not chunk:
                break
            total += len(chunk)
            if total > record["size"]:
                raise ValueError("identity_binary_size_changed")
            digest.update(chunk)
    if total != record["size"] or digest.hexdigest() != record["sha256"]:
        raise ValueError("identity_binary_hash_mismatch")
    return entries + [(origin, "bin/instance-identity", record, 0o755)]


def _archive(path, entries):
    with path.open("xb") as output:
        os.chmod(output.fileno(), 0o600)
        with tarfile.open(fileobj=output, mode="w", format=tarfile.USTAR_FORMAT) as archive:
            for origin, name, record, mode in entries:
                with file_descriptor(origin.parent, origin.name) as (fd, info):
                    if info.st_nlink != 1 or info.st_size != record["size"]:
                        raise ValueError("identity_upload_input_changed")
                    header = tarfile.TarInfo(name)
                    header.size, header.mode = info.st_size, mode
                    header.uid = header.gid = header.mtime = 0
                    with os.fdopen(os.dup(fd), "rb") as source:
                        archive.addfile(header, source)


def _binding(label):
    return {"account_hash": label * 32, "slot_id": "slot-" + label,
            "node_id": "node-lab", "epoch": 1, "generation": 1}


def public_result(text, binding, *, request=False):
    value = json.loads(text, object_pairs_hook=_object)
    if not isinstance(value, dict) or set(value) != ({"public", "csr_pem"} if request else {"public"}):
        raise ValueError("identity_public_result_rejected")
    public = value["public"]
    if (not isinstance(public, dict) or set(public) != {"binding", "machine_id", "public_key_sha256"}
            or json.dumps(public["binding"], sort_keys=True) != json.dumps(binding, sort_keys=True)
            or not isinstance(public["machine_id"], str) or not re.fullmatch(r"[a-f0-9]{32}", public["machine_id"])
            or not isinstance(public["public_key_sha256"], str) or not re.fullmatch(r"[a-f0-9]{64}", public["public_key_sha256"])):
        raise ValueError("identity_public_result_rejected")
    if request:
        csr = value["csr_pem"]
        match = re.fullmatch(r"-----BEGIN CERTIFICATE REQUEST-----\n([A-Za-z0-9+/=\n]+)-----END CERTIFICATE REQUEST-----\n", csr) if isinstance(csr, str) and len(csr) <= 16 * 1024 else None
        if match is None:
            raise ValueError("identity_csr_result_rejected")
        base64.b64decode(match[1].replace("\n", ""), validate=True)
    return value


def _remaining(deadline, maximum=30):
    remaining = int(deadline - time.monotonic())
    if remaining < 1:
        raise TimeoutError("identity_probe_deadline")
    return min(maximum, remaining)


def _command(lab, cid, operation, binding, deadline, *, denied=False):
    # This temporary stdin contains only the public binding, never private state.
    with tempfile.TemporaryFile(dir=lab.root) as source:
        source.write(json.dumps(binding).encode("ascii"))
        source.seek(0)
        result = bounded_process(lab.prefix + ["exec", "--user", "1000:1000", "-i", cid,
            "/usr/bin/timeout", "--kill-after=1", "5", BINARY, operation, DIRECTORY],
            environment(lab.config), _remaining(deadline, 10), 32 * 1024,
            check_budget=lab.capacity, input_fd=source.fileno())
    if denied:
        if result.returncode != 1 or result.stdout != "" or result.stderr != DENIED:
            raise ValueError("identity_wrong_binding_not_rejected")
        return None
    if result.returncode or result.stderr:
        raise ValueError("identity_operation_failed")
    return public_result(result.stdout, binding, request=operation == "request")


def _cleanup(lab, containers, baseline, *, unresolved_create=False, evidence_name="identity-cleanup.json"):
    if evidence_name not in ("identity-cleanup.json", "live-cleanup.json"):
        raise ValueError("identity_cleanup_evidence_rejected")
    removed, failed = [], False
    for cid in containers.values():
        try:
            value = lab.owned(cid)
            if value["State"]["Running"]:
                lab.run("stop", "--time", "5", cid, timeout=10)
            lab.owned(cid)
            lab.run("rm", cid, timeout=10)
            removed.append(cid)
        except Exception:
            # Still clean the other independently owned instance. Never broaden
            # cleanup to names, images, networks or volumes after a failure.
            failed = True
    try:
        unchanged = lab.baseline() == baseline
    except Exception:
        unchanged = False
    complete = not failed and not unresolved_create and len(removed) == len(containers) and unchanged
    lab.save(evidence_name, {"removed_container_ids": removed,
        "existing_containers_unchanged": unchanged, "cleanup_complete": complete,
        "unresolved_create": unresolved_create})
    if not complete:
        raise ValueError("identity_cleanup_failed")


def probe(lab):
    if "avx2" not in Path("/proc/cpuinfo").read_text():
        raise ValueError("identity_host_cpu_mismatch")
    available = int(next(line.split()[1] for line in Path("/proc/meminfo").read_text().splitlines()
                         if line.startswith("MemAvailable:")))
    if available < 3 * 1024**2:
        raise ValueError("identity_host_memory_reserve")
    entries = inputs(lab)
    archive = lab.root / "identity-inputs.tar"
    _archive(archive, entries)
    baseline = lab.baseline()
    lab.save("identity-baseline.json", baseline)
    containers, results = {}, {}
    unresolved_create = False
    # Both containers retain the inherited PID1 240s watchdog. Work shares a
    # shorter deadline to leave time for exact cleanup, even after failed exec.
    deadline = time.monotonic() + 200
    try:
        for label in ("a", "b"):
            args = probe_args(lab.name, memory_gib=1)
            args[args.index("--name") + 1] = lab.name + "-identity-" + label
            # The daemon may create before the client times out or loses its
            # response. An unacknowledged ID is never proof of no resource.
            unresolved_create = True
            cid = lab.run(*args, timeout=_remaining(deadline)).stdout.strip()
            if not re.fullmatch(r"[a-f0-9]{64}", cid) or cid in containers.values():
                raise ValueError("identity_container_id_rejected")
            containers[label] = cid
            unresolved_create = False
            lab.save("identity-container-" + label + ".json", {"id": cid})
            validate_probe(lab.owned(cid), lab.name, memory_gib=1)
            lab.run("start", cid, timeout=_remaining(deadline))
        # Both exist before any identity is generated. Each receives the same
        # immutable code/tools, but distinct empty tmpfs homes and local keys.
        for label, cid in containers.items():
            with archive.open("rb") as source:
                uploaded = bounded_process(lab.prefix + ["exec", "--user", "0:0", "-i", cid,
                    "/bin/sh", "-ec", root_upload(memory_bytes=1024**3)], environment(lab.config),
                    _remaining(deadline, 60), 1024 * 1024, check_budget=lab.capacity, input_fd=source.fileno())
            if uploaded.returncode or uploaded.stdout != "root-upload-capless-pass\n":
                raise ValueError("identity_upload_failed")
            validate_probe(lab.owned(cid), lab.name, memory_gib=1)
            setup = cli.probe_script(entries, smoke=False, memory_bytes=1024**3) + (
                "umask 077\nmkdir -m 700 " + DIRECTORY + "\n"
                "touch /home/claude/.identity-marker-" + label + "\n")
            lab.run("exec", "--user", "1000:1000", cid, "/usr/bin/timeout", "--kill-after=1", "10",
                    "/bin/sh", "-ec", setup, timeout=_remaining(deadline, 15))
            binding = _binding(label)
            original = _command(lab, cid, "init", binding, deadline)
            for operation in ("init", "show", "request"):
                value = _command(lab, cid, operation, binding, deadline)
                if value["public"] != original["public"]:
                    raise ValueError("identity_not_stable")
                if operation == "request":
                    results[label] = value
            for field, replacement in (("account_hash", "c" * 32), ("slot_id", "slot-other"),
                    ("node_id", "node-other"), ("epoch", 2), ("generation", 2)):
                wrong = {**binding, field: replacement}
                _command(lab, cid, "init", wrong, deadline, denied=True)
                if _command(lab, cid, "show", binding, deadline) != original:
                    raise ValueError("identity_changed_after_rejection")
            lab.save("identity-public-" + label + ".json", results[label])
        for field in ("machine_id", "public_key_sha256"):
            if results["a"]["public"][field] == results["b"]["public"][field]:
                raise ValueError("identity_shared_between_instances")
        for label, cid in containers.items():
            other = "b" if label == "a" else "a"
            markers = "test -f /home/claude/.identity-marker-" + label + "; test ! -e /home/claude/.identity-marker-" + other
            lab.run("exec", "--user", "1000:1000", cid, "/bin/sh", "-ec", markers, timeout=_remaining(deadline, 10))
            name = "identity-cli-" + label
            lab.logged(name, ["exec", "--user", "1000:1000", cid, "/usr/bin/timeout", "--kill-after=5", "70",
                "/bin/sh", "-ec", cli.probe_script(entries, require_empty_home=False, memory_bytes=1024**3)],
                timeout=_remaining(deadline, 85), stop_on_abort=cid)
            value = lab.owned(cid)
            if value["State"]["OOMKilled"] or not value["State"]["Running"]:
                raise ValueError("identity_cli_container_failed")
            if not (lab.root / (name + ".log")).read_text().rstrip().endswith(CLI_PASS):
                raise ValueError("identity_cli_incomplete")
    finally:
        _cleanup(lab, containers, baseline, unresolved_create=unresolved_create)
    lab.save("identity-result.json", {"two_local_identities": True, "stable_same_binding": True,
        "wrong_binding_fields_rejected": 5, "private_home_markers_isolated": True,
        "cli_five_checks_per_instance": True, "real_model_requests": 0,
        "authenticated_enrollment": False, "mtls_installed": False,
        "restricted_egress_verified": False, "production_ready": False})


if __name__ == "__main__":
    if len(sys.argv) != 2:
        raise SystemExit("explicit private lab root required")
    try:
        probe(Lab(sys.argv[1]))
    except Exception:
        raise SystemExit("identity_probe_failed; inspect private bounded evidence")
