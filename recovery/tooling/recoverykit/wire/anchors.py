"""Read and verify static artifact bytes, then validate source anchors."""

from __future__ import annotations

import hashlib
from typing import Any, Dict, Mapping, Tuple

from ..evidence.errors import EvidenceError
from ..evidence.filesystem import absolute_path
from ..evidence.manifest import read_entry
from ..evidence import load_manifest
from .errors import WireError


def load_artifacts(manifest_path: object, source_root: object) -> Tuple[Dict[str, Any], Dict[str, bytes]]:
    """Return one verified in-memory copy of every explicitly allowlisted artifact.

    ``read_entry`` provides the no-follow, regular-file, size, SHA-256, UTF-8
    and recognisable-secret checks.  Files are read once so anchor validation is
    against the same bytes whose artifact hash was verified.
    """

    try:
        manifest = load_manifest(manifest_path)
        # Static source must be an explicit private location; resolving a
        # relative working-directory input would make review provenance opaque.
        root = absolute_path(source_root, required=True)
    except EvidenceError as error:
        raise WireError("invalid_manifest") from error

    artifacts: Dict[str, bytes] = {}
    try:
        for entry in manifest["entries"]:
            artifacts[entry["path"]] = read_entry(entry, root)
    except EvidenceError as error:
        raise WireError("source_integrity") from error
    return manifest, artifacts


def validate_anchor(anchor: Mapping[str, Any], artifacts: Mapping[str, bytes]) -> None:
    """Confirm a reviewed byte range is UTF-8, hashed and contains its literal.

    This establishes only source anchoring.  It deliberately does not parse or
    execute JavaScript, and it does not interpret the caller's statement.
    """

    source = artifacts.get(anchor["artifact"])
    if source is None:
        raise WireError("invalid_anchor")
    start = anchor["start"]
    end = anchor["end"]
    if not 0 <= start < end <= len(source):
        raise WireError("invalid_anchor")
    span = source[start:end]
    if hashlib.sha256(span).hexdigest() != anchor["sha256"]:
        raise WireError("invalid_anchor")
    try:
        span.decode("utf-8")
    except UnicodeError as error:
        raise WireError("invalid_anchor") from error
    literal = anchor["literal"].encode("utf-8")
    if literal not in span:
        raise WireError("invalid_anchor")
