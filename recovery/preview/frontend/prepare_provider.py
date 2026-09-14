"""Create one byte-pinned, scrubbed visual-only copy; never execute original JS.

Two independently reviewed placeholder string literals are replaced in full.
The quarantined original is unchanged and never returned to a browser.
"""

import argparse
from pathlib import Path
import sys

from recovery.collectors.frontend.capture import digest
from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import (
    absolute_path, create_private_destination, json_bytes, load_json_bytes,
    private_inventory, read_regular, write_exclusive,
)
from recoverykit.evidence.policy import check_content


SOURCE_PATH = "quarantine/assets/_dashboard.providers-ABk0JmwU.js"
ARTIFACT_PATH = "files/assets/_dashboard.providers-ABk0JmwU.js"
SOURCE_SIZE = 96456
SOURCE_SHA = "72377588f9e0e4e15d97c1f9df038fa495052ab0e10051df62d1d72daedbf413"
PREVIEW_SIZE = 96445
PREVIEW_SHA = "4557e455c42f51b10bff2a75a187c7c762c9cca279c6d2333ee2796038a8da24"
PATCHES = (
    (36927, 36960, "0f8759763e8890e541b2dd99acda5b0fe2ebced639b9111d95159a660d5b7e36"),
    (37160, 37196, "1df533e379f7460a7d477dd7ab12c594732dc0456d6057ea64e7aa883c6be0b7"),
)
REPLACEMENT = b'"http://preview.invalid:8080"'


def receipt_document():
    return {
        "schema_version": 1, "kind": "ccmax.provider.visual-preview-copy",
        "source_path": SOURCE_PATH, "source_sha256": SOURCE_SHA, "source_bytes": SOURCE_SIZE,
        "artifact_path": ARTIFACT_PATH, "artifact_sha256": PREVIEW_SHA, "artifact_bytes": PREVIEW_SIZE,
        "changes": [{"start_byte": start, "end_byte": end, "source_literal_sha256": sha,
                     "context": "placeholder string literal", "replacement": REPLACEMENT.decode()}
                    for start, end, sha in PATCHES],
        "scope": "loopback visual preview only; no backend writes or external requests",
        "original_quarantine_modified": False, "business_compatibility_verified": False,
        "production_approved": False,
    }


def sanitize(raw):
    if len(raw) != SOURCE_SIZE or digest(raw) != SOURCE_SHA:
        raise EvidenceError("provider source pin mismatch")
    candidate = raw
    for start, end, sha in reversed(PATCHES):
        if digest(raw[start:end]) != sha:
            raise EvidenceError("provider literal pin mismatch")
        candidate = candidate[:start] + REPLACEMENT + candidate[end:]
    if len(candidate) != PREVIEW_SIZE or digest(candidate) != PREVIEW_SHA:
        raise EvidenceError("provider candidate pin mismatch")
    check_content(candidate, text_only=True)
    return candidate


def load_prepared(directory):
    root = absolute_path(directory, required=True)
    receipt = load_json_bytes(read_regular(root, "receipt.json", 65536))
    if receipt != receipt_document() or private_inventory(root) != {"receipt.json", ARTIFACT_PATH}:
        raise EvidenceError("provider preview receipt mismatch")
    raw = read_regular(root, ARTIFACT_PATH, PREVIEW_SIZE)
    if len(raw) != PREVIEW_SIZE or digest(raw) != PREVIEW_SHA:
        raise EvidenceError("provider preview content mismatch")
    check_content(raw, text_only=True)
    return raw


def prepare(capture_root, destination):
    source = absolute_path(capture_root, required=True)
    destination = absolute_path(destination, required=True)
    raw = read_regular(source, SOURCE_PATH, SOURCE_SIZE)
    candidate = sanitize(raw)
    create_private_destination(destination, (source, Path(__file__).resolve().parents[3]))
    write_exclusive(destination, ARTIFACT_PATH, candidate)
    write_exclusive(destination, "receipt.json", json_bytes(receipt_document()))
    load_prepared(destination)
    return {"status": "verified", "source_unchanged": True, "placeholder_literals_replaced": 2,
            "preview_bytes": PREVIEW_SIZE, "preview_sha256": PREVIEW_SHA, "production_approved": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--capture-root", required=True)
    parser.add_argument("--destination", required=True)
    args = parser.parse_args()
    try:
        print(json_bytes(prepare(args.capture_root, args.destination)).decode(), end="")
        return 0
    except (EvidenceError, OSError, ValueError, TypeError):
        print("Provider preview preparation failed; no source content printed", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
