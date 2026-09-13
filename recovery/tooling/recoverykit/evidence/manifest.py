"""Validate explicit evidence manifests before accessing their source files."""

from __future__ import annotations

import hashlib
import re

from .errors import EvidenceError
from .filesystem import absolute_path, json_bytes, load_json_bytes, read_regular, relative_path
from .policy import (MAX_ENTRIES, MAX_FILE_BYTES, MAX_METADATA_BYTES, MAX_TOTAL_BYTES,
                     check_content, check_evidence_path)


def validate_manifest(manifest: object) -> dict:
    if not isinstance(manifest, dict) or set(manifest) != {"schema_version", "id", "entries"}:
        raise EvidenceError("manifest requires schema_version, id and entries")
    if type(manifest["schema_version"]) is not int or manifest["schema_version"] != 1:
        raise EvidenceError("unsupported manifest schema version")
    identity = manifest["id"]
    if not isinstance(identity, str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", identity):
        raise EvidenceError("invalid manifest id")
    check_content(identity.encode("utf-8"), text_only=True)
    entries = manifest["entries"]
    if not isinstance(entries, list) or not 1 <= len(entries) <= MAX_ENTRIES:
        raise EvidenceError("manifest entries must be a bounded non-empty list")
    checked = []
    names = set()
    total = 0
    for entry in entries:
        if not isinstance(entry, dict) or set(entry) != {"path", "size", "sha256", "source"}:
            raise EvidenceError("entry requires path, size, sha256 and source")
        name = relative_path(entry["path"])
        check_evidence_path(name)
        key = name.casefold()
        if key in names or any(key.startswith(p + "/") or p.startswith(key + "/") for p in names):
            raise EvidenceError("duplicate or overlapping manifest paths")
        names.add(key)
        size = entry["size"]
        digest = entry["sha256"]
        source = entry["source"]
        if type(size) is not int or not 0 <= size <= MAX_FILE_BYTES:
            raise EvidenceError("invalid evidence size")
        if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise EvidenceError("invalid SHA-256")
        if not isinstance(source, str) or not source.strip() or len(source) > 2048:
            raise EvidenceError("source description is required")
        check_content(source.encode("utf-8"), text_only=True)
        total += size
        if total > MAX_TOTAL_BYTES:
            raise EvidenceError("manifest exceeds the total byte limit")
        checked.append(dict(entry))
    normalized = {"schema_version": 1, "id": identity, "entries": checked}
    if len(json_bytes(normalized)) > MAX_METADATA_BYTES:
        raise EvidenceError("canonical manifest exceeds metadata size limit")
    return normalized


def load_manifest(path) -> dict:
    path = absolute_path(path)
    data = read_regular(path.parent, path.name, MAX_METADATA_BYTES)
    return validate_manifest(load_json_bytes(data))


def read_entry(entry: dict, source_root) -> bytes:
    content = read_regular(source_root, entry["path"], MAX_FILE_BYTES)
    if len(content) != entry["size"] or hashlib.sha256(content).hexdigest() != entry["sha256"]:
        raise EvidenceError("evidence size or SHA-256 mismatch: " + entry["path"])
    check_content(content, text_only=True)
    return content


def verify_manifest(manifest: dict, source_root) -> dict:
    manifest = validate_manifest(manifest)
    source_root = absolute_path(source_root)
    for entry in manifest["entries"]:
        read_entry(entry, source_root)
    return {
        "status": "verified", "schema_version": 1, "manifest_id": manifest["id"],
        "file_count": len(manifest["entries"]),
        "total_bytes": sum(entry["size"] for entry in manifest["entries"]),
        "entries": [{key: entry[key] for key in ("path", "size", "sha256")}
                    for entry in manifest["entries"]],
    }
