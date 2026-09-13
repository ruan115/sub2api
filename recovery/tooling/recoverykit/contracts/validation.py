"""Semantic self-consistency checks for recovery contract declarations."""

from __future__ import annotations

import re
from typing import Any, Dict, List, Mapping, Optional, Sequence, Tuple

from .schema import DISCOVERED_COLLECTIONS, OWNERS, SCHEMA_VERSION, STATUSES


_STABLE_ID = re.compile(r"^[a-z][a-z0-9_.-]*$")


def validate_manifest(
    document: Mapping[str, Any],
    relative_path: str,
    issues: List[str],
    record_ids: Dict[str, str],
    route_keys: Dict[Tuple[str, str], str],
) -> Optional[Dict[str, Any]]:
    """Validate one manifest and return a compact summary when it is usable."""

    location = relative_path
    owner_from_path = relative_path.split("/", 1)[0]
    _require_exact_schema_version(document, location, issues)
    module_id = _required_stable_id(document, "id", location, issues)
    owner = _required_string(document, "owner", location, issues)
    status = _required_status(document, location, issues)
    evidence = _validate_evidence(document.get("evidence"), location, issues)
    unknowns = _validate_unknowns(document.get("unknowns"), location, issues)
    discovered = document.get("discovered")

    owner_is_valid = owner in OWNERS if owner is not None else False
    owner_matches_path = owner == owner_from_path if owner is not None else False
    if owner is not None and not owner_is_valid:
        issues.append("%s: owner must be one of %s" % (location, ", ".join(sorted(OWNERS))))
    if owner is not None and not owner_matches_path:
        issues.append("%s: owner %r does not match directory owner %r" % (location, owner, owner_from_path))
    _validate_status_evidence(status, unknowns, document, evidence, location, issues)
    if module_id is not None:
        _record_unique_id(module_id, location, record_ids, issues)

    if not isinstance(discovered, Mapping):
        issues.append("%s: discovered must be a JSON object" % location)
        return None
    unknown_collections = sorted(set(discovered) - DISCOVERED_COLLECTIONS)
    for collection in unknown_collections:
        issues.append("%s.discovered: unsupported collection %r" % (location, collection))

    entry_count = 0
    unknown_count = len(unknowns or ())
    entry_statuses: List[str] = []
    for collection in sorted(DISCOVERED_COLLECTIONS):
        records = discovered.get(collection, [])
        collection_location = "%s.discovered.%s" % (location, collection)
        if not isinstance(records, list):
            issues.append("%s: must be a JSON array" % collection_location)
            continue
        for index, entry in enumerate(records):
            entry_location = "%s[%d]" % (collection_location, index)
            entry_count += 1
            if not isinstance(entry, Mapping):
                issues.append("%s: entry must be a JSON object" % entry_location)
                continue
            entry_id = _required_stable_id(entry, "id", entry_location, issues)
            entry_status = _required_status(entry, entry_location, issues)
            entry_evidence = _validate_evidence_references(
                entry.get("evidence"), evidence, entry_location, issues
            )
            entry_unknowns = _validate_unknowns(entry.get("unknowns"), entry_location, issues)
            _validate_status_evidence(
                entry_status, entry_unknowns, entry, evidence, entry_location, issues
            )
            if entry_id is not None:
                _record_unique_id(entry_id, entry_location, record_ids, issues)
            if entry_status is not None:
                entry_statuses.append(entry_status)
            unknown_count += len(entry_unknowns or ())

            if collection == "api_paths":
                _validate_route(entry, entry_location, entry_id, route_keys, issues)
            elif collection == "pages":
                _validate_page(entry, entry_location, issues)
            if entry_evidence is not None and not entry_evidence:
                issues.append("%s: evidence must name at least one manifest evidence ID" % entry_location)

    if entry_count == 0:
        issues.append("%s: discovered must contain at least one record" % location)
    if module_id is None or owner is None or status is None or not owner_is_valid or not owner_matches_path:
        return None
    return {
        "id": module_id,
        "owner": owner,
        "status": status,
        "entry_count": entry_count,
        "unknown_count": unknown_count,
        "entry_statuses": entry_statuses,
    }


def _require_exact_schema_version(document: Mapping[str, Any], location: str, issues: List[str]) -> None:
    if document.get("schema_version") != SCHEMA_VERSION:
        issues.append("%s: schema_version must be %d" % (location, SCHEMA_VERSION))


def _required_string(
    document: Mapping[str, Any], key: str, location: str, issues: List[str]
) -> Optional[str]:
    value = document.get(key)
    if not isinstance(value, str) or not value.strip():
        issues.append("%s: %s must be a non-empty string" % (location, key))
        return None
    return value


def _required_stable_id(
    document: Mapping[str, Any], key: str, location: str, issues: List[str]
) -> Optional[str]:
    value = _required_string(document, key, location, issues)
    if value is not None and not _STABLE_ID.match(value):
        issues.append("%s: %s must be a lowercase stable ID" % (location, key))
        return None
    return value


def _required_status(document: Mapping[str, Any], location: str, issues: List[str]) -> Optional[str]:
    status = _required_string(document, "status", location, issues)
    if status is not None and status not in STATUSES:
        issues.append("%s: unsupported status %r" % (location, status))
        return None
    return status


def _validate_evidence(value: Any, location: str, issues: List[str]) -> Dict[str, Mapping[str, Any]]:
    if not isinstance(value, list) or not value:
        issues.append("%s: evidence must be a non-empty JSON array" % location)
        return {}
    records: Dict[str, Mapping[str, Any]] = {}
    for index, record in enumerate(value):
        evidence_location = "%s.evidence[%d]" % (location, index)
        if not isinstance(record, Mapping):
            issues.append("%s: evidence entry must be a JSON object" % evidence_location)
            continue
        evidence_id = _required_stable_id(record, "id", evidence_location, issues)
        for key in ("kind", "source", "observed"):
            _required_string(record, key, evidence_location, issues)
        if evidence_id is not None:
            if evidence_id in records:
                issues.append("%s: duplicate evidence ID %r" % (evidence_location, evidence_id))
            else:
                records[evidence_id] = record
    return records


def _validate_evidence_references(
    value: Any,
    evidence: Mapping[str, Mapping[str, Any]],
    location: str,
    issues: List[str],
) -> Optional[List[str]]:
    if not isinstance(value, list) or not value:
        issues.append("%s: evidence must be a non-empty JSON array of evidence IDs" % location)
        return None
    references: List[str] = []
    for index, evidence_id in enumerate(value):
        reference_location = "%s.evidence[%d]" % (location, index)
        if not isinstance(evidence_id, str) or not evidence_id:
            issues.append("%s: evidence ID must be a non-empty string" % reference_location)
            continue
        if evidence_id not in evidence:
            issues.append("%s: unknown evidence ID %r" % (reference_location, evidence_id))
        references.append(evidence_id)
    return references


def _validate_unknowns(value: Any, location: str, issues: List[str]) -> Optional[List[str]]:
    if not isinstance(value, list):
        issues.append("%s: unknowns must be a JSON array" % location)
        return None
    unknowns: List[str] = []
    for index, unknown in enumerate(value):
        if not isinstance(unknown, str) or not unknown.strip():
            issues.append("%s.unknowns[%d]: must be a non-empty string" % (location, index))
            continue
        unknowns.append(unknown)
    return unknowns


def _validate_status_evidence(
    status: Optional[str],
    unknowns: Optional[Sequence[str]],
    record: Mapping[str, Any],
    evidence: Mapping[str, Mapping[str, Any]],
    location: str,
    issues: List[str],
) -> None:
    if status == "discovered" and not unknowns:
        issues.append("%s: discovered records must declare at least one unknown" % location)
    if status == "implemented":
        _require_evidence_list(record, "implementation_evidence", evidence, location, issues)
    if status == "verified":
        _require_evidence_list(record, "verification_evidence", evidence, location, issues)


def _require_evidence_list(
    record: Mapping[str, Any],
    key: str,
    evidence: Mapping[str, Mapping[str, Any]],
    location: str,
    issues: List[str],
) -> None:
    value = record.get(key)
    if not isinstance(value, list) or not value:
        issues.append("%s: %s must be a non-empty array of evidence IDs" % (location, key))
        return
    for index, evidence_id in enumerate(value):
        if not isinstance(evidence_id, str) or not evidence_id or evidence_id not in evidence:
            issues.append(
                "%s.%s[%d]: must reference a declared evidence ID" % (location, key, index)
            )


def _record_unique_id(identifier: str, location: str, seen: Dict[str, str], issues: List[str]) -> None:
    previous = seen.get(identifier)
    if previous is not None:
        issues.append("%s: duplicate stable ID %r (already used by %s)" % (location, identifier, previous))
        return
    seen[identifier] = location


def _validate_route(
    entry: Mapping[str, Any],
    location: str,
    entry_id: Optional[str],
    seen: Dict[Tuple[str, str], str],
    issues: List[str],
) -> None:
    path = _required_string(entry, "path", location, issues)
    method = entry.get("method", "unknown")
    if path is not None and not path.startswith("/"):
        issues.append("%s: API path must start with '/'" % location)
    if not isinstance(method, str) or not method:
        issues.append("%s: method must be a non-empty string or omitted" % location)
        return
    if path is None or entry_id is None:
        return
    normalized_method = method.upper()
    for (existing_path, existing_method), previous in seen.items():
        if existing_path == path and (
            existing_method == normalized_method
            or existing_method == "UNKNOWN"
            or normalized_method == "UNKNOWN"
        ):
            issues.append(
                "%s: API path conflict for %s %s (already used by %s)"
                % (location, normalized_method, path, previous)
            )
            return
    seen[(path, normalized_method)] = entry_id


def _validate_page(entry: Mapping[str, Any], location: str, issues: List[str]) -> None:
    path = _required_string(entry, "path", location, issues)
    if path is not None and not path.startswith("/"):
        issues.append("%s: page path must start with '/'" % location)
