"""Make a private, synthetic-test-only copy of frozen source and verified tools.

This is deliberately separate from release-source staging: tests never enter
that artifact. No install scripts, workspace snapshots or Git filters run.
"""
import hashlib
import json
import os
from pathlib import Path
import tarfile

from artifacts.source import load_lock, verify_source
from lab.host import bounded_process, environment
from recoverykit.evidence.filesystem import file_descriptor
from lab.toolchain import TESTS, checksums

COMMIT = "e2715b6e7f968e638c2f4fd68467c56fa0151c72"
PREFIX = "execution-plane/isthmus-runtime/"
LOCKS = Path(__file__).resolve().parents[1] / "locks"
ROOT_UPLOAD = """set -eu
test "$(id -u):$(id -g)" = 0:0
test "$(ls /sys/class/net)" = lo
test "$(cat /sys/fs/cgroup/memory.max)" = 2147483648
awk '/^CapEff:/ {if ($2 != "0000000000000000") exit 1; cap=1} /^NoNewPrivs:/ {if ($2 != 1) exit 1; nnp=1} END {if (!cap || !nnp) exit 1}' /proc/self/status
echo root-upload-capless-pass
exec /bin/tar --no-same-owner --no-same-permissions -xf - -C /opt/isthmus-probe
"""


def expected_records():
    """Reviewed repository locks are trust anchors, not payload-side receipts."""
    lock = load_lock(LOCKS / "app-fake-source-2026-09-17.json")
    testlock = json.loads((LOCKS / "probe-tests-source-2026-09-17.json").read_text())
    binarylock = json.loads((LOCKS / "toolchain-linux-amd64-expanded-2026-09-17.json").read_text())
    if (testlock["commit"] != COMMIT or [e["path"] for e in testlock["files"]] != list(TESTS)
            or binarylock["platform"] != "linux/amd64"
            or [e["file"] for e in binarylock["binaries"]] != ["bun-1.3.9", "bun-1.4.2", "claude"]):
        raise ValueError("probe_reviewed_lock_mismatch")
    result = [{"path": "app/" + e["path"], "size": e["size_bytes"], "sha256": e["sha256"]}
              for e in lock["files"]]
    result += [{"path": "app/" + e["path"], "size": e["size"], "sha256": e["sha256"]}
               for e in testlock["files"]]
    result += [{"path": "bin/" + e["file"], "size": e["size"], "sha256": e["sha256"]}
               for e in binarylock["binaries"]]
    return sorted(result, key=lambda item: item["path"])


def tests_from_git(repo, dest):
    """Local only. Explicit list of synthetic tests, pinned to the source commit."""
    dest = Path(dest)
    dest.mkdir(mode=0o700)
    manifest = []
    for path in TESTS:
        # No shell, checkout, filters, credentials, config, replace refs or lazy fetch.
        result = bounded_process(["/usr/bin/git", "--no-replace-objects", "-C", str(repo),
            "cat-file", "blob", COMMIT + ":" + PREFIX + path],
            env={"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                 "GIT_NO_LAZY_FETCH": "1", "GIT_TERMINAL_PROMPT": "0", "GIT_ALLOW_PROTOCOL": ""},
            timeout=10, limit=128 * 1024)
        if result.returncode:
            raise ValueError("synthetic_test_git_failed")
        data = result.stdout.encode("utf-8")
        if not 1 <= len(data) <= 128 * 1024:
            raise ValueError("synthetic_test_size_rejected")
        target = dest / path
        target.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
        with target.open("xb") as out:
            os.chmod(out.fileno(), 0o600)
            out.write(data)
        manifest.append({"path": path, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
    with (dest / "tests.json").open("x") as out:
        os.chmod(out.fileno(), 0o600)
        json.dump({"commit": COMMIT, "files": manifest}, out, sort_keys=True)


def prepare_payload(lab):
    source = lab.root / "frozen-source"
    verify_source(source)
    lock = load_lock(LOCKS / "app-fake-source-2026-09-17.json")
    if load_lock(source / "source.lock.json") != lock:
        raise ValueError("probe_unreviewed_source_lock")
    if lock["source_commit"] != COMMIT:
        raise ValueError("probe_source_commit_mismatch")
    tests = lab.root / "frozen-tests"
    testlock = json.loads((LOCKS / "probe-tests-source-2026-09-17.json").read_text())
    if json.loads((tests / "tests.json").read_text()) != testlock:
        raise ValueError("probe_unreviewed_tests_lock")
    if testlock["commit"] != COMMIT or [entry["path"] for entry in testlock["files"]] != list(TESTS):
        raise ValueError("probe_test_allowlist_mismatch")
    binaries = json.loads((LOCKS / "toolchain-linux-amd64-expanded-2026-09-17.json").read_text())["binaries"]
    if sorted(entry["file"] for entry in binaries) != ["bun-1.3.9", "bun-1.4.2", "claude"]:
        raise ValueError("probe_binary_allowlist_mismatch")
    payload = lab.root / "payload"
    payload.mkdir(mode=0o755)
    entries = []
    entries += [(source / "source" / e["path"], "app/" + e["path"], e["size_bytes"], e["sha256"], 0o644)
                for e in lock["files"]]
    entries += [(tests / e["path"], "app/" + e["path"], e["size"], e["sha256"], 0o644)
                for e in testlock["files"]]
    entries += [(lab.root / "toolchain-bin" / e["file"], "bin/" + e["file"], e["size"],
                 e["sha256"], 0o755) for e in binaries]
    records = []
    for origin, path, size, digest, mode in entries:
        if (origin.is_symlink() or not origin.is_file() or origin.stat().st_nlink != 1
                or origin.stat().st_size != size):
            raise ValueError("probe_input_rejected")
        dest = payload / path
        dest.parent.mkdir(mode=0o755, parents=True, exist_ok=True)
        actual = hashlib.sha256()
        with origin.open("rb") as inp, dest.open("xb") as out:
            os.chmod(out.fileno(), mode)
            total = 0
            while True:
                chunk = inp.read(min(1024 * 1024, size + 1 - total))
                if not chunk:
                    break
                total += len(chunk)
                if total > size:
                    raise ValueError("probe_input_size_exceeded")
                out.write(chunk)
                actual.update(chunk)
        if total != size or actual.hexdigest() != digest:
            raise ValueError("probe_input_digest_mismatch")
        records.append({"path": path, "size": size, "sha256": digest})
    # Outer task directory remains0700. The copied app is public source, not home.
    for directory, _, _ in os.walk(payload):
        os.chmod(directory, 0o755)
    lab.save("probe-payload.json", sorted(records, key=lambda item: item["path"]))
    checksums(lab.root)


def upload_payload(lab, cid):
    """No Docker cp: its readonly-root guard also rejects runtime tmpfs paths.

Only system tar runs as root inside this capless container, to populate its
root-owned scratch mount. Bun/CLI/test execution always uses configured UID1000.
The archive has only reviewed regular files, no links or ambient tar metadata.
"""
    records = checksums(lab.root)
    archive = lab.root / "probe-payload.tar"
    with archive.open("xb") as out:
        os.chmod(out.fileno(), 0o600)
        with tarfile.open(fileobj=out, mode="w", format=tarfile.USTAR_FORMAT) as tar:
            for record in records:
                with file_descriptor(lab.root / "payload", record["path"]) as (fd, info):
                    if info.st_size != record["size"] or info.st_nlink != 1:
                        raise ValueError("probe_upload_input_changed")
                    header = tarfile.TarInfo(record["path"])
                    header.size = record["size"]
                    header.mode = 0o755 if record["path"].startswith("bin/") else 0o644
                    header.uid = header.gid = header.mtime = 0
                    with os.fdopen(os.dup(fd), "rb") as source:
                        tar.addfile(header, source)
    lab.owned(cid)
    with archive.open("rb") as source:
        result = bounded_process(lab.prefix + ["exec", "--user", "0:0", "-i", cid,
            "/bin/sh", "-ec", ROOT_UPLOAD],
            environment(lab.config), 90, 1024 * 1024, check_budget=lab.capacity, input_fd=source.fileno())
    if result.returncode or result.stdout != "root-upload-capless-pass\n":
        raise ValueError("probe_payload_upload_failed")
    lab.save("toolchain-upload.json", {"archive_bytes": archive.stat().st_size,
        "files": len(records), "root_system_tar_only": True, "cap_eff_zero": True,
        "no_new_privileges": True, "network_none": True})
