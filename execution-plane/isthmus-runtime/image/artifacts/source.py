"""Freeze only the reviewed fake application's Git blobs, never a worktree.

Staging checks membership in the fixed commit. Independent verification checks
the complete private output against its lock, not Git provenance or a signature.
Neither operation executes source or grants permission to run a workload.
"""

import hashlib
import os
from pathlib import PurePosixPath
import re
import stat
import time

from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import (
    absolute_path, create_private_destination, file_descriptor, json_bytes,
    load_json_bytes, open_directory, read_regular,
)


SOURCE_COMMIT = "e2715b6e7f968e638c2f4fd68467c56fa0151c72"
SOURCE_ROOT = "execution-plane/isthmus-runtime"
SOURCE_PATHS = (
    "contracts/grpc/messages.proto", "package.json",
    "src/app/fake.ts", "src/app/serve.ts", "src/app/shutdown.ts",
    "src/protocol/websocket/codec.ts", "src/protocol/websocket/errors.ts",
    "src/protocol/websocket/index.ts", "src/protocol/websocket/tags.ts",
    "src/protocol/websocket/validation.ts", "src/runtime/turn/cancellation.ts",
    "src/runtime/turn/errors.ts", "src/runtime/turn/fake.ts",
    "src/runtime/turn/request.ts", "src/runtime/turn/types.ts",
    "src/transport/http/handler.ts", "src/transport/http/local-access.ts",
    "src/transport/http/response.ts", "src/transport/shared/body.ts",
    "src/transport/shared/errors.ts", "src/transport/websocket/controls.ts",
    "src/transport/websocket/session.ts", "src/transport/websocket/writer.ts",
)
MAX_FILE_BYTES = 128 * 1024
MAX_TOTAL_BYTES = 2 * 1024 * 1024
MAX_METADATA_BYTES = 64 * 1024
GIT_TIMEOUT_SECONDS = 60
_LOCK_FIELDS = frozenset({"schema_version", "kind", "capability", "source_commit",
                          "source_root", "file_count", "total_bytes", "files"})
_FILE_FIELDS = frozenset({"path", "mode", "git_blob_oid", "size_bytes", "sha256"})
_ENVIRONMENT = {"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C",
                "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null",
                "GIT_NO_REPLACE_OBJECTS": "1", "GIT_NO_LAZY_FETCH": "1",
                "GIT_ALLOW_PROTOCOL": "",
                "GIT_TERMINAL_PROMPT": "0", "GIT_OPTIONAL_LOCKS": "0"}


class SourceError(ValueError):
    """Fixed categories only, with no source bytes or private paths in errors."""


def validate_lock(value):
    if not isinstance(value, dict) or set(value) != _LOCK_FIELDS:
        raise SourceError("source_lock_schema")
    if (type(value["schema_version"]) is not int or value["schema_version"] != 1
            or value["kind"] != "isthmus-fake-source" or value["capability"] != "fake-only"
            or value["source_commit"] != SOURCE_COMMIT or value["source_root"] != SOURCE_ROOT
            or type(value["file_count"]) is not int or value["file_count"] != len(SOURCE_PATHS)
            or type(value["total_bytes"]) is not int
            or not 0 < value["total_bytes"] <= MAX_TOTAL_BYTES
            or not isinstance(value["files"], list) or len(value["files"]) != len(SOURCE_PATHS)):
        raise SourceError("source_lock_schema")
    records = []
    for expected_path, record in zip(SOURCE_PATHS, value["files"]):
        if not isinstance(record, dict) or set(record) != _FILE_FIELDS:
            raise SourceError("source_lock_schema")
        if (record["path"] != expected_path or record["mode"] != "100644"
                or type(record["size_bytes"]) is not int
                or not 0 < record["size_bytes"] <= MAX_FILE_BYTES
                or not isinstance(record["git_blob_oid"], str)
                or re.fullmatch(r"[0-9a-f]{40}", record["git_blob_oid"]) is None
                or not isinstance(record["sha256"], str)
                or re.fullmatch(r"[0-9a-f]{64}", record["sha256"]) is None):
            raise SourceError("source_lock_schema")
        records.append(dict(record))
    if sum(record["size_bytes"] for record in records) != value["total_bytes"]:
        raise SourceError("source_total_mismatch")
    return {**value, "files": records}


def _file_identity(info, *, private=False):
    if (not stat.S_ISREG(info.st_mode) or info.st_nlink != 1
            or private and (info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600)):
        raise SourceError("source_file_rejected")
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def _current_file(root, name, *, private=False):
    with file_descriptor(root, name) as (_, info):
        return _file_identity(info, private=private)


def _directory_identity(root, *, private=False):
    with open_directory(root) as fd:
        info = os.fstat(fd)
        if private and (info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700):
            raise SourceError("source_directory_rejected")
        return info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid


def _assert_directory(root, identity, *, private=False):
    if _directory_identity(root, private=private) != identity:
        raise SourceError("source_directory_changed")


def _read(root, name, limit, *, private=False):
    identity = _current_file(root, name, private=private)
    data = read_regular(root, name, limit)
    if _current_file(root, name, private=private) != identity:
        raise SourceError("source_file_changed")
    return data


def load_lock(path):
    try:
        path = absolute_path(path, required=True)
        return validate_lock(load_json_bytes(_read(path.parent, path.name, MAX_METADATA_BYTES)))
    except SourceError:
        raise
    except (EvidenceError, OSError, TypeError, ValueError):
        raise SourceError("source_lock_unavailable") from None


def _git(repo, args, *, limit, deadline):
    # Independent verification needs no Git/process module, including on the
    # isolated native probe where only verification helpers are copied.
    from recoverykit.workspace.process import ProcessError, run_bounded

    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise SourceError("source_git_failed")
    command = ["/usr/bin/git", "--no-pager", "--no-replace-objects",
               "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
               "-c", "protocol.allow=never", "-C", str(repo), *args]
    try:
        result = run_bounded(command, environment=dict(_ENVIRONMENT), stdout_limit=limit,
                             stderr_limit=16 * 1024, timeout_seconds=remaining)
        if result.returncode != 0:
            raise SourceError("source_git_failed")
        return result.stdout
    except ProcessError:
        raise SourceError("source_git_failed") from None


def _check_bytes(record, data):
    if (len(data) != record["size_bytes"] or hashlib.sha256(data).hexdigest() != record["sha256"]
            or hashlib.sha1(b"blob " + str(len(data)).encode("ascii") + b"\0" + data).hexdigest()
            != record["git_blob_oid"]):
        raise SourceError("source_bytes_mismatch")


def _collect(repo, lock):
    deadline = time.monotonic() + GIT_TIMEOUT_SECONDS
    def git(*args, limit=MAX_METADATA_BYTES):
        return _git(repo, list(args), limit=limit, deadline=deadline)
    if (git("rev-parse", "--show-toplevel") != os.fsencode(repo) + b"\n"
            or git("rev-parse", "--is-bare-repository") != b"false\n"
            or git("rev-parse", "--verify", SOURCE_COMMIT + "^{commit}") != SOURCE_COMMIT.encode() + b"\n"):
        raise SourceError("source_repository_rejected")
    paths = [SOURCE_ROOT + "/" + path for path in SOURCE_PATHS]
    tree = git("ls-tree", "-rz", "--long", "--full-tree", SOURCE_COMMIT, "--", *paths)
    entries = tree.split(b"\0")
    if len(entries) != len(SOURCE_PATHS) + 1 or entries[-1] != b"":
        raise SourceError("source_git_tree_mismatch")
    found = {}
    for entry in entries[:-1]:
        try:
            metadata, path = entry.decode("ascii").split("\t")
            mode, kind, oid, size = metadata.split()
            if path in found or mode != "100644" or kind != "blob" or not size.isdecimal():
                raise ValueError
            found[path] = (oid, int(size))
        except (ValueError, UnicodeError):
            raise SourceError("source_git_tree_mismatch") from None
    if set(found) != set(paths):
        raise SourceError("source_git_tree_mismatch")
    blobs = {}
    for record in lock["files"]:
        if found[SOURCE_ROOT + "/" + record["path"]] != (record["git_blob_oid"], record["size_bytes"]):
            raise SourceError("source_git_tree_mismatch")
        data = git("cat-file", "blob", record["git_blob_oid"], limit=record["size_bytes"])
        _check_bytes(record, data)
        blobs[record["path"]] = data
    return blobs


def _inventory(root, *, complete):
    files = {"source/" + path for path in SOURCE_PATHS} | {"source.lock.json"}
    if complete:
        files.add("receipt.json")
    directories = {""}
    for name in files:
        directories.update(str(p) for p in PurePosixPath(name).parents if str(p) != ".")
    proofs = {}
    for name in sorted(directories):
        directory = root / name
        proofs[name + "/"] = _directory_identity(directory, private=True)
        expected = {p.rsplit("/", 1)[-1] for p in files | directories
                    if p and str(PurePosixPath(p).parent) == (name or ".")}
        with open_directory(directory) as fd, os.scandir(fd) as entries:
            seen = set()
            for entry in entries:
                if entry.name not in expected or entry.name in seen:
                    raise SourceError("source_inventory_mismatch")
                seen.add(entry.name)
            if seen != expected:
                raise SourceError("source_inventory_mismatch")
    for name in files:
        proofs[name] = _current_file(root, name, private=True)
    return proofs


def _receipt(lock):
    return {"schema_version": 1, "kind": "isthmus-fake-source-artifact", "status": "complete",
            "capability": "fake-only", "execution_permitted": False,
            "source_commit": SOURCE_COMMIT, "source_root": SOURCE_ROOT,
            "lock_sha256": hashlib.sha256(json_bytes(lock)).hexdigest(),
            "file_count": lock["file_count"], "total_bytes": lock["total_bytes"]}


def _verify(destination, *, complete):
    proofs = _inventory(destination, complete=complete)
    raw = _read(destination, "source.lock.json", MAX_METADATA_BYTES, private=True)
    lock = validate_lock(load_json_bytes(raw))
    if raw != json_bytes(lock):
        raise SourceError("source_lock_not_canonical")
    for record in lock["files"]:
        _check_bytes(record, _read(destination, "source/" + record["path"], record["size_bytes"], private=True))
    receipt = _receipt(lock)
    if complete and _read(destination, "receipt.json", MAX_METADATA_BYTES, private=True) != json_bytes(receipt):
        raise SourceError("source_receipt_mismatch")
    if _inventory(destination, complete=complete) != proofs:
        raise SourceError("source_inventory_changed")
    return receipt


def _report(receipt):
    return {"status": "fake_source_verified", "capability": "fake-only",
            "execution_permitted": False, "source_commit": SOURCE_COMMIT,
            "file_count": receipt["file_count"], "total_bytes": receipt["total_bytes"]}


def _write(root, name, content):
    # Only our fixed inventory reaches this writer. Unlike a generic loop,
    # zero-progress writes must fail rather than retaining an operation forever.
    parts = name.split("/")
    with open_directory(root) as root_fd:
        current, fd = os.dup(root_fd), None
        try:
            for part in parts[:-1]:
                try:
                    os.mkdir(part, 0o700, dir_fd=current)
                except FileExistsError:
                    pass
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=current)
                os.close(current)
                current = child
                info = os.fstat(child)
                if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
                    raise SourceError("source_directory_rejected")
            fd = os.open(parts[-1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600, dir_fd=current)
            os.fchmod(fd, 0o600)
            remaining = memoryview(content)
            while remaining:
                written = os.write(fd, remaining)
                if written <= 0:
                    raise SourceError("source_write_failed")
                remaining = remaining[written:]
            os.fsync(fd)
        finally:
            if fd is not None:
                os.close(fd)
            os.close(current)


def stage_source(repo, lock_path, dest):
    """Write a new private destination; leave partial artifacts on any error.

The caller must independently verify before use. A receipt left by a final I/O
failure is not proof that staging returned success. No worktree files are read.
"""
    try:
        repo, lock_path, dest = (absolute_path(p, required=True) for p in (repo, lock_path, dest))
        repo_identity = _directory_identity(repo)
        input_identity = _current_file(lock_path.parent, lock_path.name)
        lock = load_lock(lock_path)
        blobs = _collect(repo, lock)
        if _current_file(lock_path.parent, lock_path.name) != input_identity:
            raise SourceError("source_lock_changed")
        _assert_directory(repo, repo_identity)
        expected = _receipt(lock)
        create_private_destination(dest, (repo,))
        root_identity = _directory_identity(dest, private=True)
        for name, data in [("source.lock.json", json_bytes(lock)),
                           *(("source/" + path, blobs[path]) for path in SOURCE_PATHS)]:
            _assert_directory(dest, root_identity, private=True)
            _write(dest, name, data)
        _assert_directory(dest, root_identity, private=True)
        if _verify(dest, complete=False) != expected:
            raise SourceError("source_inputs_changed")
        _write(dest, "receipt.json", json_bytes(expected))
        if _verify(dest, complete=True) != expected:
            raise SourceError("source_inputs_changed")
        if _current_file(lock_path.parent, lock_path.name) != input_identity:
            raise SourceError("source_lock_changed")
        _assert_directory(repo, repo_identity)
        _assert_directory(dest, root_identity, private=True)
        return _report(expected)
    except SourceError:
        raise
    except (EvidenceError, OSError, TypeError, ValueError):
        raise SourceError("source_stage_failed") from None


def verify_source(dest):
    try:
        return _report(_verify(absolute_path(dest, required=True), complete=True))
    except SourceError:
        raise
    except (EvidenceError, OSError, TypeError, ValueError):
        raise SourceError("source_verification_failed") from None
