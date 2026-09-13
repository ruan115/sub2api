"""Descriptor-bound, no-follow filesystem primitives for contract catalogs.

The catalog is review metadata, but it is still an input boundary: never use a
path discovered during inventory for a later path-based read.  These helpers
keep each directory and manifest bound to the descriptor that was inspected.
"""

from __future__ import annotations

import contextlib
import os
from pathlib import Path
import stat
from typing import Iterator, List, Tuple

from ..evidence.errors import EvidenceError
from ..evidence.filesystem import absolute_path, open_directory


MAX_MANIFESTS = 256
MAX_MANIFEST_BYTES = 1024 * 1024
MAX_CATALOG_BYTES = 16 * 1024 * 1024
MAX_DIRECTORY_ENTRIES = 512


class CatalogFilesystemError(ValueError):
    """A catalog path was unavailable, unsafe, or changed during inspection."""


class CatalogInputLimitError(CatalogFilesystemError):
    """A bounded catalog filesystem input exceeded a fixed safety budget."""


def catalog_path(root: object) -> Path:
    """Return an absolute spelling without resolving (and thus following) links."""

    try:
        path = absolute_path(root)
    except (EvidenceError, OSError, TypeError, ValueError) as error:
        raise CatalogFilesystemError() from error
    # Do this before opening the root so malformed spellings cannot surface a
    # platform-specific ``ValueError`` past the fixed loader error boundary.
    if any(
        ord(character) < 32 or ord(character) == 127
        for component in path.parts
        for character in component
    ):
        raise CatalogFilesystemError()
    return path


@contextlib.contextmanager
def open_catalog_root(root: object) -> Iterator[Tuple[Path, int]]:
    """Open every root ancestor with ``O_NOFOLLOW`` and yield its directory fd."""

    path = catalog_path(root)
    try:
        with open_directory(path) as descriptor:
            _require_directory(os.fstat(descriptor))
            # Do not catch exceptions from the caller's catalog parse or
            # validation work.  ``ContractError`` is a ValueError by design
            # and must retain its own stable category.
            yield path, descriptor
    except CatalogFilesystemError:
        raise
    except (EvidenceError, OSError) as error:
        raise CatalogFilesystemError() from error


def directory_entries(descriptor: int) -> List[str]:
    """Enumerate a bounded directory through a duplicate of an open fd."""

    scan_descriptor = None
    iterator = None
    names: List[str] = []
    try:
        scan_descriptor = os.dup(descriptor)
        iterator = os.scandir(scan_descriptor)
        for entry in iterator:
            name = entry.name
            if not _safe_component(name):
                raise CatalogFilesystemError()
            names.append(name)
            if len(names) > MAX_DIRECTORY_ENTRIES:
                raise CatalogInputLimitError()
    except CatalogFilesystemError:
        raise
    except OSError as error:
        raise CatalogFilesystemError() from error
    finally:
        if iterator is not None:
            iterator.close()
        if scan_descriptor is not None:
            try:
                os.close(scan_descriptor)
            except OSError:
                pass
    return sorted(names)


def entry_stat(parent_descriptor: int, name: str):
    """Return a child lstat through an already-open parent directory."""

    if not _safe_component(name):
        raise CatalogFilesystemError()
    try:
        return os.stat(name, dir_fd=parent_descriptor, follow_symlinks=False)
    except OSError as error:
        raise CatalogFilesystemError() from error


def open_child_directory(parent_descriptor: int, name: str) -> Tuple[int, object]:
    """Open one checked child directory without following a renamed/link target."""

    expected = entry_stat(parent_descriptor, name)
    _require_directory(expected)
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | getattr(os, "O_CLOEXEC", 0)
    descriptor = None
    retained = False
    try:
        descriptor = os.open(name, flags, dir_fd=parent_descriptor)
        actual = os.fstat(descriptor)
        _require_directory(actual)
        if not _same_node(expected, actual):
            raise CatalogFilesystemError()
        ensure_entry_unchanged(parent_descriptor, name, actual, directory=True)
        retained = True
        return descriptor, actual
    except CatalogFilesystemError:
        raise
    except OSError as error:
        raise CatalogFilesystemError() from error
    finally:
        if descriptor is not None and not retained:
            try:
                os.close(descriptor)
            except OSError:
                pass


def close_descriptor(descriptor: int) -> None:
    try:
        os.close(descriptor)
    except OSError as error:
        raise CatalogFilesystemError() from error


def read_named_regular(parent_descriptor: int, name: str, limit: int) -> bytes:
    """Read one stable, regular child from the same no-follow descriptor chain."""

    if type(limit) is not int or limit < 0:
        raise CatalogFilesystemError()
    expected = entry_stat(parent_descriptor, name)
    _require_regular(expected)
    flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | getattr(os, "O_CLOEXEC", 0)
    descriptor = None
    try:
        descriptor = os.open(name, flags, dir_fd=parent_descriptor)
        opened = os.fstat(descriptor)
        _require_regular(opened)
        if not _same_node(expected, opened):
            raise CatalogFilesystemError()
        ensure_entry_unchanged(parent_descriptor, name, opened, directory=False)
        data = read_regular_descriptor(descriptor, limit)
        ensure_entry_unchanged(parent_descriptor, name, opened, directory=False)
        return data
    except CatalogFilesystemError:
        raise
    except OSError as error:
        raise CatalogFilesystemError() from error
    finally:
        if descriptor is not None:
            try:
                os.close(descriptor)
            except OSError:
                pass


def read_regular_descriptor(descriptor: int, limit: int) -> bytes:
    """Read a regular fd with a size cap and before/after snapshot fencing."""

    try:
        before = os.fstat(descriptor)
        _require_regular(before)
        if before.st_size > limit:
            raise CatalogInputLimitError()
        chunks = []
        total = 0
        while True:
            chunk = os.read(descriptor, min(1024 * 1024, limit + 1 - total))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > limit:
                raise CatalogInputLimitError()
        after = os.fstat(descriptor)
        if not _same_snapshot(before, after):
            raise CatalogFilesystemError()
        return b"".join(chunks)
    except CatalogFilesystemError:
        raise
    except OSError as error:
        raise CatalogFilesystemError() from error


def directory_snapshot(descriptor: int):
    try:
        value = os.fstat(descriptor)
        _require_directory(value)
        return value
    except CatalogFilesystemError:
        raise
    except OSError as error:
        raise CatalogFilesystemError() from error


def ensure_directory_stable(descriptor: int, before: object) -> None:
    try:
        after = os.fstat(descriptor)
        _require_directory(after)
        if not _same_snapshot(before, after):
            raise CatalogFilesystemError()
    except CatalogFilesystemError:
        raise
    except OSError as error:
        raise CatalogFilesystemError() from error


def ensure_entry_unchanged(parent_descriptor: int, name: str, expected: object, *, directory: bool) -> None:
    current = entry_stat(parent_descriptor, name)
    if directory:
        _require_directory(current)
    else:
        _require_regular(current)
    if not _same_node(expected, current):
        raise CatalogFilesystemError()


def _safe_component(value: object) -> bool:
    return (
        isinstance(value, str)
        and value not in ("", ".", "..")
        and "/" not in value
        and "\\" not in value
        and not any(ord(character) < 32 or ord(character) == 127 for character in value)
    )


def _require_directory(value: object) -> None:
    if not stat.S_ISDIR(value.st_mode):
        raise CatalogFilesystemError()


def _require_regular(value: object) -> None:
    if not stat.S_ISREG(value.st_mode):
        raise CatalogFilesystemError()


def _same_node(left: object, right: object) -> bool:
    return left.st_dev == right.st_dev and left.st_ino == right.st_ino


def _same_snapshot(left: object, right: object) -> bool:
    return (
        _same_node(left, right)
        and left.st_size == right.st_size
        and left.st_mtime_ns == right.st_mtime_ns
        and left.st_ctime_ns == right.st_ctime_ns
    )
