"""Compose source-anchor and same-memory catalog-reference validation."""

from __future__ import annotations

from typing import Any, Dict, Mapping, Tuple

from ..contracts.loader import ContractError, load_catalog
from ..contracts.validation import validate_manifest
from .anchors import load_artifacts, validate_anchor
from .errors import WireError
from .schema import load_observation_document


def validate_observations(
    document_path: object,
    manifest_path: object,
    source_root: object,
    catalog_root: object,
) -> Dict[str, Any]:
    """Validate reviewed, source-bound observations without changing a catalog.

    Every input is explicit and read only.  Success says only that the reviewed
    statements are anchored to verified static source bytes and to existing API
    record IDs; it never establishes that a statement is semantically correct
    or that the old service is compatible.
    """

    document = load_observation_document(document_path)
    manifest, artifacts = load_artifacts(manifest_path, source_root)
    if document["artifact_manifest_id"] != manifest["id"]:
        raise WireError("invalid_manifest")
    api_records = _load_api_records_once(catalog_root)

    record_ids = set()
    anchor_count = 0
    for observation in document["observations"]:
        record_path = api_records.get(observation["record_id"])
        if record_path is None or record_path != observation["path"]:
            raise WireError("catalog_reference")
        record_ids.add(observation["record_id"])
        for anchor in observation["anchors"]:
            validate_anchor(anchor, artifacts)
            anchor_count += 1

    return {
        "status": "source_anchored",
        "business_verification": False,
        "counts": {
            "artifacts": len(artifacts),
            "observations": len(document["observations"]),
            "records": len(record_ids),
            "anchors": anchor_count,
        },
    }


def _load_api_records_once(catalog_root: object) -> Dict[str, str]:
    """Load and fully validate a catalog once, then index that same memory image."""

    try:
        catalog = load_catalog(catalog_root)
    except ContractError as error:
        raise WireError("catalog_reference") from error

    issues = []
    record_ids: Dict[str, str] = {}
    route_keys: Dict[Tuple[str, str], str] = {}
    for loaded in catalog["manifests"]:
        relative_path = loaded["path"]
        document = loaded["data"]
        if not isinstance(document, Mapping):
            issues.append("manifest")
            continue
        validate_manifest(document, relative_path, issues, record_ids, route_keys)
    if issues:
        raise WireError("catalog_reference")

    api_records: Dict[str, str] = {}
    for loaded in catalog["manifests"]:
        document = loaded["data"]
        discovered = document["discovered"]
        for entry in discovered.get("api_paths", []):
            # The same-memory validation above guarantees these keys and their
            # global stable-ID uniqueness before this index is constructed.
            api_records[entry["id"]] = entry["path"]
    return api_records
