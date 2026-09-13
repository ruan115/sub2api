"""Descriptor-bound JSON loading for the offline recovery contract catalog."""

from __future__ import annotations

import json
import os
import stat
from typing import Any, Dict, List, Sequence, Union

from .filesystem import (MAX_CATALOG_BYTES, MAX_MANIFESTS, MAX_MANIFEST_BYTES,
                         CatalogFilesystemError, CatalogInputLimitError,
                         close_descriptor, directory_entries, directory_snapshot,
                         ensure_directory_stable, ensure_entry_unchanged, entry_stat,
                         open_catalog_root, open_child_directory, read_named_regular)
from .schema import MANIFEST_FILENAME, OWNERS


PathLike = Union[str, os.PathLike]

_FILESYSTEM_ERROR = "catalog filesystem safety check failed"
_LIMIT_ERROR = "catalog input exceeds safety limits"
_LAYOUT_ERROR = "catalog layout is invalid"
_JSON_ERROR = "catalog contains invalid JSON"
_DUPLICATE_JSON_ERROR = "catalog contains a duplicate JSON key"


class ContractError(ValueError):
    """Raised when a catalog cannot be loaded or is internally inconsistent."""

    def __init__(self, issues: Union[str, Sequence[str]]) -> None:
        if isinstance(issues, str):
            issues = (issues,)
        self.issues = tuple(str(issue) for issue in issues)
        super().__init__("Contract catalog validation failed:\n- " + "\n- ".join(self.issues))


class _DuplicateJSONKey(ValueError):
    """Private parser error that prevents JSON's default last-key-wins behavior."""


def load_catalog(root: PathLike) -> Dict[str, Any]:
    """Load a bounded catalog through a no-follow descriptor tree.

    Only ``<root>/<owner>/<module>/manifest.json`` is accepted.  Every root
    ancestor and every catalog component is opened with ``O_NOFOLLOW``; each
    manifest is inventory-checked and read through the same regular file
    descriptor, so a path cannot be swapped after inventory and reopened.
    This function loads JSON only.  Call :func:`validate_catalog` for semantic
    consistency checks.
    """

    try:
        with open_catalog_root(root) as (catalog_root, root_descriptor):
            manifests = _load_root(root_descriptor)
    except CatalogInputLimitError as error:
        raise ContractError(_LIMIT_ERROR) from error
    except CatalogFilesystemError as error:
        raise ContractError(_FILESYSTEM_ERROR) from error
    return {"root": str(catalog_root), "manifests": manifests}


def _load_root(root_descriptor: int) -> List[Dict[str, Any]]:
    before = directory_snapshot(root_descriptor)
    manifests: List[Dict[str, Any]] = []
    total_bytes = [0]
    for owner in directory_entries(root_descriptor):
        information = entry_stat(root_descriptor, owner)
        if _is_link_or_special(information):
            raise CatalogFilesystemError()
        if owner not in OWNERS or not _is_directory(information):
            raise ContractError(_LAYOUT_ERROR)
        owner_descriptor, owner_identity = open_child_directory(root_descriptor, owner)
        try:
            _load_owner(owner_descriptor, owner, manifests, total_bytes)
            ensure_directory_stable(owner_descriptor, owner_identity)
        finally:
            close_descriptor(owner_descriptor)
        ensure_entry_unchanged(root_descriptor, owner, owner_identity, directory=True)
    ensure_directory_stable(root_descriptor, before)
    if not manifests:
        raise ContractError("catalog must contain at least one manifest")
    return manifests


def _load_owner(
    owner_descriptor: int,
    owner: str,
    manifests: List[Dict[str, Any]],
    total_bytes: List[int],
) -> None:
    before = directory_snapshot(owner_descriptor)
    for module in directory_entries(owner_descriptor):
        information = entry_stat(owner_descriptor, module)
        if _is_link_or_special(information):
            raise CatalogFilesystemError()
        if not _is_directory(information):
            raise ContractError(_LAYOUT_ERROR)
        if len(manifests) >= MAX_MANIFESTS:
            raise CatalogInputLimitError()
        remaining = MAX_CATALOG_BYTES - total_bytes[0]
        if remaining <= 0:
            raise CatalogInputLimitError()
        module_descriptor, module_identity = open_child_directory(owner_descriptor, module)
        try:
            raw = _read_module_manifest(module_descriptor, min(MAX_MANIFEST_BYTES, remaining))
            total_bytes[0] += len(raw)
            manifests.append(
                {
                    "path": "%s/%s/%s" % (owner, module, MANIFEST_FILENAME),
                    "data": _parse_manifest(raw),
                }
            )
            ensure_directory_stable(module_descriptor, module_identity)
        finally:
            close_descriptor(module_descriptor)
        ensure_entry_unchanged(owner_descriptor, module, module_identity, directory=True)
    ensure_directory_stable(owner_descriptor, before)


def _read_module_manifest(module_descriptor: int, limit: int) -> bytes:
    before = directory_snapshot(module_descriptor)
    names = directory_entries(module_descriptor)
    if names != [MANIFEST_FILENAME]:
        for name in names:
            if _is_link_or_special(entry_stat(module_descriptor, name)):
                raise CatalogFilesystemError()
        raise ContractError(_LAYOUT_ERROR)
    raw = read_named_regular(module_descriptor, MANIFEST_FILENAME, limit)
    ensure_directory_stable(module_descriptor, before)
    return raw


def _parse_manifest(raw: bytes) -> Any:
    try:
        text = raw.decode("utf-8")
        return json.loads(text, object_pairs_hook=_object_without_duplicate_keys)
    except _DuplicateJSONKey as error:
        raise ContractError(_DUPLICATE_JSON_ERROR) from error
    except (UnicodeError, ValueError, RecursionError) as error:
        raise ContractError(_JSON_ERROR) from error


def _object_without_duplicate_keys(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise _DuplicateJSONKey()
        value[key] = item
    return value


def _is_directory(information: object) -> bool:
    return stat.S_ISDIR(information.st_mode)


def _is_link_or_special(information: object) -> bool:
    return stat.S_ISLNK(information.st_mode) or not (
        stat.S_ISDIR(information.st_mode) or stat.S_ISREG(information.st_mode)
    )
