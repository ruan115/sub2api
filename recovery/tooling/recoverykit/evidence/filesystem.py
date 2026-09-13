"""No-follow, exclusive, owner-only filesystem operations for local evidence."""

from __future__ import annotations

import contextlib
import json
import os
from pathlib import Path, PurePosixPath
import stat
import unicodedata

from .errors import EvidenceError


def relative_path(value: object) -> str:
    if not isinstance(value, str) or not value or len(value) > 1024:
        raise EvidenceError("invalid relative path")
    if ("\\" in value or any(ord(c) < 32 or ord(c) == 127 for c in value)
            or unicodedata.normalize("NFC", value) != value):
        raise EvidenceError("unsafe relative path")
    parts = value.split("/")
    if any(p in ("", ".", "..") for p in parts) or ":" in parts[0]:
        raise EvidenceError("unsafe relative path")
    if PurePosixPath(value).is_absolute():
        raise EvidenceError("absolute evidence paths are forbidden")
    return value


def absolute_path(value: object, *, required: bool = False) -> Path:
    try:
        path = Path(value)
    except (TypeError, ValueError) as exc:
        raise EvidenceError("invalid filesystem path") from exc
    if required and not path.is_absolute():
        raise EvidenceError("an explicit absolute path is required")
    if ".." in path.parts:
        raise EvidenceError("parent traversal is forbidden")
    return Path(os.path.abspath(path))


def within(path: Path, root: Path) -> bool:
    return path == root or root in path.parents


@contextlib.contextmanager
def open_directory(path: Path):
    """Open each path component separately so no ancestor follows a symlink."""
    path = absolute_path(path, required=True)
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    fd = os.open(path.anchor, flags)
    try:
        for part in path.parts[1:]:
            child = os.open(part, flags, dir_fd=fd)
            os.close(fd)
            fd = child
        yield fd
    except OSError as exc:
        raise EvidenceError("directory is unavailable or contains a symlink") from exc
    finally:
        os.close(fd)


@contextlib.contextmanager
def file_descriptor(root: Path, name: str):
    parts = relative_path(name).split("/")
    with open_directory(root) as root_fd:
        current = os.dup(root_fd)
        fd = None
        try:
            for part in parts[:-1]:
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                dir_fd=current)
                os.close(current)
                current = child
            fd = os.open(parts[-1], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK,
                         dir_fd=current)
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode):
                raise EvidenceError("evidence must be a regular file")
            yield fd, info
        except OSError as exc:
            raise EvidenceError("file is unavailable, not regular, or contains a symlink") from exc
        finally:
            if fd is not None:
                os.close(fd)
            os.close(current)


def read_regular(root: Path, name: str, limit: int) -> bytes:
    with file_descriptor(root, name) as (fd, before):
        if before.st_size > limit:
            raise EvidenceError("file exceeds the permitted size")
        chunks = []
        total = 0
        while True:
            chunk = os.read(fd, min(1024 * 1024, limit + 1 - total))
            if not chunk:
                break
            chunks.append(chunk)
            total += len(chunk)
            if total > limit:
                raise EvidenceError("file exceeds the permitted size")
        after = os.fstat(fd)
        if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns,
                before.st_ctime_ns) != (after.st_dev, after.st_ino, after.st_size,
                                        after.st_mtime_ns, after.st_ctime_ns):
            raise EvidenceError("file changed while being read")
        return b"".join(chunks)


def create_private_destination(destination: Path, forbidden_roots=()) -> None:
    destination = absolute_path(destination, required=True)
    if any(within(destination, root) for root in forbidden_roots):
        raise EvidenceError("destination must be outside the source repository or tree")
    with open_directory(destination.parent) as parent_fd:
        for ancestor in (destination.parent, *destination.parent.parents):
            if os.path.lexists(ancestor / ".git"):
                raise EvidenceError("private artifacts must be outside Git worktrees")
        try:
            os.mkdir(destination.name, 0o700, dir_fd=parent_fd)
        except FileExistsError as exc:
            raise EvidenceError("destination already exists; overwriting is forbidden") from exc
        except OSError as exc:
            raise EvidenceError("cannot create private destination") from exc
    with open_directory(destination) as fd:
        os.fchmod(fd, 0o700)


def write_exclusive(root: Path, name: str, content: bytes) -> None:
    parts = relative_path(name).split("/")
    with open_directory(root) as root_fd:
        current = os.dup(root_fd)
        fd = None
        try:
            for part in parts[:-1]:
                try:
                    os.mkdir(part, 0o700, dir_fd=current)
                except FileExistsError:
                    pass
                child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                dir_fd=current)
                info = os.fstat(child)
                if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
                    os.close(child)
                    raise EvidenceError("artifact directory is not owner-only")
                os.close(current)
                current = child
            fd = os.open(parts[-1], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600, dir_fd=current)
            os.fchmod(fd, 0o600)
            view = memoryview(content)
            while view:
                written = os.write(fd, view)
                view = view[written:]
            os.fsync(fd)
        except OSError as exc:
            raise EvidenceError("exclusive artifact write failed") from exc
        finally:
            if fd is not None:
                os.close(fd)
            os.close(current)


def json_bytes(value: object) -> bytes:
    return (json.dumps(value, ensure_ascii=True, sort_keys=True, indent=2) + "\n").encode()


def load_json_bytes(content: bytes) -> object:
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise EvidenceError("duplicate JSON member")
            result[key] = value
        return result
    try:
        return json.loads(content.decode("utf-8"), object_pairs_hook=unique)
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise EvidenceError("invalid JSON metadata") from exc


def private_inventory(root: Path) -> set[str]:
    """Reject links/special files and relaxed permissions anywhere in an artifact."""
    files = set()
    with open_directory(root) as root_fd:
        def walk(fd, prefix):
            info = os.fstat(fd)
            if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
                raise EvidenceError("artifact directory is not owner-only")
            for name in os.listdir(fd):
                relative = relative_path(prefix + name)
                info = os.stat(name, dir_fd=fd, follow_symlinks=False)
                if info.st_uid != os.getuid():
                    raise EvidenceError("artifact is owned by another user")
                if stat.S_ISDIR(info.st_mode):
                    child = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                                    dir_fd=fd)
                    try:
                        walk(child, relative + "/")
                    finally:
                        os.close(child)
                elif stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600:
                    files.add(relative)
                else:
                    raise EvidenceError("artifact contains a symlink, special file, or non-private file")
        try:
            walk(root_fd, "")
        except OSError as exc:
            raise EvidenceError("artifact changed or cannot be inspected") from exc
    return files
