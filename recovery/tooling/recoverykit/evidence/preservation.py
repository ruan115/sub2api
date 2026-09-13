"""Preserve verified files; the completion receipt is written last."""

from __future__ import annotations

from datetime import datetime, timezone
import hashlib

from .errors import EvidenceError
from .filesystem import (absolute_path, create_private_destination, json_bytes,
                         load_json_bytes, private_inventory, read_regular, write_exclusive)
from .manifest import read_entry, validate_manifest, verify_manifest
from .policy import MAX_METADATA_BYTES


def preserve_manifest(manifest: dict, source_root, destination) -> dict:
    manifest = validate_manifest(manifest)
    source_root = absolute_path(source_root)
    destination = absolute_path(destination, required=True)
    result = verify_manifest(manifest, source_root)
    encoded = json_bytes(manifest)
    receipt = {
        "schema_version": 1, "kind": "recovery-evidence", "status": "complete",
        "manifest_id": manifest["id"], "manifest_sha256": hashlib.sha256(encoded).hexdigest(),
        "created_at": datetime.now(timezone.utc).isoformat(),
        "file_count": result["file_count"], "total_bytes": result["total_bytes"],
        "entries": result["entries"], "secret_screening": "filename-and-pattern-v1",
    }
    receipt_bytes = json_bytes(receipt)
    if len(encoded) > MAX_METADATA_BYTES or len(receipt_bytes) > MAX_METADATA_BYTES:
        raise EvidenceError("preserved metadata exceeds size limit")
    create_private_destination(destination, (source_root,))
    # Failure during copying leaves private output without a completion receipt.
    for entry in manifest["entries"]:
        content = read_entry(entry, source_root)
        write_exclusive(destination, "files/" + entry["path"], content)
    write_exclusive(destination, "manifest.json", encoded)
    write_exclusive(destination, "receipt.json", receipt_bytes)
    return verify_preserved(destination)


def verify_preserved(destination) -> dict:
    destination = absolute_path(destination, required=True)
    inventory = private_inventory(destination)
    receipt = load_json_bytes(read_regular(destination, "receipt.json", MAX_METADATA_BYTES))
    if (not isinstance(receipt, dict) or receipt.get("schema_version") != 1
            or receipt.get("kind") != "recovery-evidence" or receipt.get("status") != "complete"):
        raise EvidenceError("missing or invalid completion receipt")
    encoded = read_regular(destination, "manifest.json", MAX_METADATA_BYTES)
    if hashlib.sha256(encoded).hexdigest() != receipt.get("manifest_sha256"):
        raise EvidenceError("preserved manifest hash mismatch")
    manifest = validate_manifest(load_json_bytes(encoded))
    expected = {"manifest.json", "receipt.json"} | {"files/" + e["path"] for e in manifest["entries"]}
    if inventory != expected:
        raise EvidenceError("preserved file inventory does not match the manifest")
    result = verify_manifest(manifest, destination / "files")
    for key in ("manifest_id", "file_count", "total_bytes", "entries"):
        if receipt.get(key) != result[key]:
            raise EvidenceError("completion receipt disagrees with manifest")
    return dict(result, destination=str(destination), kind="recovery-evidence")
