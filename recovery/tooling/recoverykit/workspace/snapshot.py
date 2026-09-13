"""Capture WIP without changing HEAD, the index, or the source worktree."""

from __future__ import annotations

from datetime import datetime, timezone
import hashlib
import os
import re
import stat

from ..evidence.errors import EvidenceError
from ..evidence.filesystem import (absolute_path, create_private_destination, file_descriptor,
                                   json_bytes, load_json_bytes, private_inventory, read_regular,
                                   relative_path, write_exclusive)
from ..evidence.policy import MAX_FILE_BYTES, MAX_METADATA_BYTES, check_content, check_sensitive_path
from .errors import WorkspaceError
from .git import MAX_GIT_BYTES, capture, decode_paths, repository_root

MAX_SNAPSHOT_BYTES = 128 * 1024 * 1024
MAX_UNTRACKED_FILES = 10000
GIT_FILES = {"git/head.txt", "git/branch.txt", "git/status.porcelain", "git/staged.patch",
             "git/unstaged.patch", "git/tracked.patch", "git/untracked.paths",
             "git/changed.paths", "git/index-changed.paths"}


def _digest(content: bytes) -> str:
    return hashlib.sha256(content).hexdigest()


def _record(path: str, content: bytes, *, mode: int = 0o600) -> dict:
    return {"path": path, "size": len(content), "sha256": _digest(content), "source_mode": mode}


def _untracked(repo, paths: list[str]) -> dict:
    if len(paths) > MAX_UNTRACKED_FILES:
        raise WorkspaceError("too many untracked files")
    found = {}
    total = 0
    for path in paths:
        check_sensitive_path(path)
        with file_descriptor(repo, path) as (_, info):
            mode = stat.S_IMODE(info.st_mode)
        data = read_regular(repo, path, MAX_FILE_BYTES)
        check_content(data, text_only=False)
        total += len(data)
        if total > MAX_SNAPSHOT_BYTES:
            raise WorkspaceError("untracked files exceed the snapshot size limit")
        found[path] = (data, mode)
    return found


def _check_tracked_paths(repo, captured: dict) -> None:
    paths = set(decode_paths(captured["git/changed.paths"]))
    paths.update(decode_paths(captured["git/index-changed.paths"]))
    for path in paths:
        check_sensitive_path(path)
        # Removed files are represented by the binary patch. Existing ones must
        # not introduce links, devices, or secret source contents into the patch.
        candidate = repo / path
        if os.path.lexists(candidate):
            content = read_regular(repo, path, MAX_FILE_BYTES)
            check_content(content, text_only=False)


def snapshot_workspace(repo, destination) -> dict:
    try:
        return _snapshot_workspace(repo, destination)
    except EvidenceError as exc:
        raise WorkspaceError(str(exc)) from exc


def _snapshot_workspace(repo, destination) -> dict:
    repo = repository_root(repo)
    destination = absolute_path(destination, required=True)
    captured = capture(repo)
    _check_tracked_paths(repo, captured)
    for content in captured.values():
        check_content(content, text_only=False)
    untracked_paths = decode_paths(captured["git/untracked.paths"])
    files = _untracked(repo, untracked_paths)
    total = sum(map(len, captured.values())) + sum(len(data) for data, _ in files.values())
    if total > MAX_SNAPSHOT_BYTES:
        raise WorkspaceError("snapshot exceeds the total byte limit")
    # Check that collection did not race an edit before allocating an artifact.
    if capture(repo) != captured:
        raise WorkspaceError("Git state changed during snapshot collection")
    records = [_record(path, content) for path, content in captured.items()]
    records.extend(_record("untracked/" + path, content, mode=mode)
                   for path, (content, mode) in files.items())
    branch = captured["git/branch.txt"].decode("utf-8").strip() or None
    manifest = {
        "schema_version": 1, "kind": "recovery-workspace", "status": "complete",
        "created_at": datetime.now(timezone.utc).isoformat(), "repository": str(repo),
        "head": captured["git/head.txt"].decode("ascii").strip(), "branch": branch,
        "file_count": len(records), "untracked_count": len(files), "total_bytes": total,
        "ignored_files_included": False, "automatic_restore": False,
        "secret_screening": "filename-and-pattern-v1", "files": records,
    }
    encoded = json_bytes(manifest)
    if len(encoded) > MAX_METADATA_BYTES * 8:
        raise WorkspaceError("canonical workspace metadata exceeds size limit")
    create_private_destination(destination, (repo,))
    for path, content in captured.items():
        write_exclusive(destination, path, content)
    for path, (content, _) in files.items():
        write_exclusive(destination, "untracked/" + path, content)
    if capture(repo) != captured or _untracked(repo, untracked_paths) != files:
        raise WorkspaceError("workspace changed during snapshot; private output is incomplete")
    write_exclusive(destination, "manifest.json", encoded)
    return verify_snapshot(destination)


def verify_snapshot(destination) -> dict:
    try:
        return _verify_snapshot(destination)
    except EvidenceError as exc:
        raise WorkspaceError(str(exc)) from exc


def _verify_snapshot(destination) -> dict:
    destination = absolute_path(destination, required=True)
    inventory = private_inventory(destination)
    manifest = load_json_bytes(read_regular(destination, "manifest.json", MAX_METADATA_BYTES * 8))
    if (not isinstance(manifest, dict) or type(manifest.get("schema_version")) is not int
            or manifest["schema_version"] != 1 or manifest.get("kind") != "recovery-workspace"
            or manifest.get("status") != "complete" or manifest.get("automatic_restore") is not False
            or manifest.get("ignored_files_included") is not False):
        raise WorkspaceError("missing or invalid workspace completion manifest")
    records = manifest.get("files")
    if not isinstance(records, list) or not len(GIT_FILES) <= len(records) <= MAX_UNTRACKED_FILES + len(GIT_FILES):
        raise WorkspaceError("invalid snapshot file inventory")
    expected = set()
    total = 0
    stored = {}
    for entry in records:
        if not isinstance(entry, dict) or set(entry) != {"path", "size", "sha256", "source_mode"}:
            raise WorkspaceError("invalid snapshot file record")
        path = relative_path(entry["path"])
        if path in expected or not (path in GIT_FILES or path.startswith("untracked/")):
            raise WorkspaceError("duplicate or unexpected snapshot path")
        if type(entry["size"]) is not int or not 0 <= entry["size"] <= MAX_GIT_BYTES:
            raise WorkspaceError("invalid snapshot file size")
        if type(entry["source_mode"]) is not int or not 0 <= entry["source_mode"] <= 0o7777:
            raise WorkspaceError("invalid snapshot source mode")
        if not isinstance(entry["sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", entry["sha256"]):
            raise WorkspaceError("invalid snapshot SHA-256")
        if path.startswith("untracked/"):
            check_sensitive_path(path[len("untracked/"):])
        data = read_regular(destination, path, MAX_GIT_BYTES)
        if len(data) != entry["size"] or _digest(data) != entry["sha256"]:
            raise WorkspaceError("snapshot file hash or size mismatch: " + path)
        check_content(data, text_only=False)
        expected.add(path)
        total += len(data)
        if total > MAX_SNAPSHOT_BYTES:
            raise WorkspaceError("snapshot exceeds the total byte limit")
        if path in GIT_FILES:
            stored[path] = data
    if not GIT_FILES <= expected or inventory != expected | {"manifest.json"}:
        raise WorkspaceError("snapshot inventory is incomplete or contains extra files")
    head = stored["git/head.txt"].strip().decode("ascii", errors="replace")
    branch = stored["git/branch.txt"].decode("utf-8").strip() or None
    if not re.fullmatch(r"[0-9a-f]{40}|[0-9a-f]{64}", head) or head != manifest.get("head"):
        raise WorkspaceError("snapshot HEAD metadata disagrees")
    if branch != manifest.get("branch"):
        raise WorkspaceError("snapshot branch metadata disagrees")
    untracked = decode_paths(stored["git/untracked.paths"])
    decode_paths(stored["git/changed.paths"])
    decode_paths(stored["git/index-changed.paths"])
    if {"untracked/" + p for p in untracked} != expected - GIT_FILES:
        raise WorkspaceError("untracked path list disagrees with snapshot files")
    if (manifest.get("file_count") != len(expected) or manifest.get("total_bytes") != total
            or manifest.get("untracked_count") != len(untracked)):
        raise WorkspaceError("snapshot summary disagrees with file inventory")
    return {"status": "verified", "kind": "recovery-workspace", "schema_version": 1,
            "destination": str(destination), "head": head, "branch": branch,
            "file_count": len(expected), "untracked_count": len(untracked), "total_bytes": total,
            "ignored_files_included": False, "automatic_restore": False}
