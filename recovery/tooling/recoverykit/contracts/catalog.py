"""Public catalog validation and JSON-serializable summary construction."""

from __future__ import annotations

from collections import Counter
from typing import Any, Dict, List, Mapping, Tuple

from .loader import PathLike, load_catalog
from .schema import OWNERS, SCHEMA_VERSION, STATUSES
from .validation import validate_manifest


def validate_catalog(root: PathLike) -> Dict[str, Any]:
    """Validate a recovery catalog and return its structural summary.

    Success means that local declarations, evidence references, IDs, statuses
    and path ownership are internally consistent.  It never establishes online
    behavior, so ``business_verification`` is always ``False``.

    Raises:
        ContractError: forwarded from the loader or validation when an issue is
            found.  Import it from :mod:`recoverykit.contracts`.
    """

    catalog = load_catalog(root)
    issues: List[str] = []
    record_ids: Dict[str, str] = {}
    route_keys: Dict[Tuple[str, str], str] = {}
    owner_counts: Dict[str, Counter] = {owner: Counter() for owner in sorted(OWNERS)}
    status_counts: Counter = Counter()
    module_summaries: List[Dict[str, Any]] = []

    for loaded in catalog["manifests"]:
        relative_path = loaded["path"]
        document = loaded["data"]
        if not isinstance(document, Mapping):
            issues.append("%s: manifest must be a JSON object" % relative_path)
            continue
        module = validate_manifest(
            document=document,
            relative_path=relative_path,
            issues=issues,
            record_ids=record_ids,
            route_keys=route_keys,
        )
        if module is None:
            continue
        _accumulate_module(module, owner_counts, status_counts, module_summaries, relative_path)

    if issues:
        from .loader import ContractError

        raise ContractError(issues)
    return _build_report(catalog, owner_counts, status_counts, module_summaries)


def _accumulate_module(
    module: Mapping[str, Any],
    owner_counts: Mapping[str, Counter],
    status_counts: Counter,
    module_summaries: List[Dict[str, Any]],
    relative_path: str,
) -> None:
    owner = module["owner"]
    status = module["status"]
    entry_count = module["entry_count"]
    unknown_count = module["unknown_count"]
    counts = owner_counts[owner]
    counts["manifest_count"] += 1
    counts["entry_count"] += entry_count
    counts["unknown_count"] += unknown_count
    counts["status:%s" % status] += 1
    status_counts[status] += 1
    for entry_status in module["entry_statuses"]:
        counts["status:%s" % entry_status] += 1
        status_counts[entry_status] += 1
    module_summaries.append(
        {
            "id": module["id"],
            "owner": owner,
            "path": relative_path,
            "status": status,
            "entry_count": entry_count,
            "unknown_count": unknown_count,
        }
    )


def _build_report(
    catalog: Mapping[str, Any],
    owner_counts: Mapping[str, Counter],
    status_counts: Counter,
    module_summaries: List[Dict[str, Any]],
) -> Dict[str, Any]:
    owners_report: Dict[str, Dict[str, Any]] = {}
    for owner in sorted(OWNERS):
        counts = owner_counts[owner]
        owners_report[owner] = {
            "manifest_count": counts["manifest_count"],
            "entry_count": counts["entry_count"],
            "unknown_count": counts["unknown_count"],
            "statuses": {
                status: counts["status:%s" % status]
                for status in sorted(STATUSES)
                if counts["status:%s" % status]
            },
        }
    module_summaries.sort(key=lambda module: module["id"])
    return {
        "schema_version": SCHEMA_VERSION,
        "valid": True,
        "business_verification": False,
        "validation_scope": "catalog structure and internal references only",
        "root": catalog["root"],
        "manifest_count": len(module_summaries),
        "entry_count": sum(module["entry_count"] for module in module_summaries),
        "unknown_count": sum(module["unknown_count"] for module in module_summaries),
        "owners": owners_report,
        "statuses": {status: status_counts[status] for status in sorted(status_counts)},
        "modules": module_summaries,
    }
