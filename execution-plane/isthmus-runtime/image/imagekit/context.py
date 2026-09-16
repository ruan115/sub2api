"""Private offline build inputs, never package execution or a build permit.

Hashes bind caller-reviewed bytes, not an official signature or installability.
The small ar-header check does not parse control.tar, dependencies or scripts.
"""

import hashlib
import os
import stat

from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import (
    absolute_path, create_private_destination, file_descriptor, json_bytes,
    load_json_bytes, open_directory, read_regular, write_exclusive,
)

from .lock import ImageError, MAX_FILE_BYTES, MAX_TOTAL_BYTES, load_lock, validate_lock
from .recipe import render_checksums, render_recipe


_CHUNK = 1024 * 1024
_METADATA_LIMIT = 256 * 1024
_ROOT_FILES = frozenset({"Dockerfile", "packages.sha256", "artifacts.lock.json"})


def _file_identity(info):
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ImageError("package_file_type_rejected")
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def _current_file(root, name):
    with file_descriptor(root, name) as (_, info):
        return _file_identity(info)


def _directory_identity(path, *, private=False):
    with open_directory(path) as fd:
        info = os.fstat(fd)
        if private and (info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700):
            raise ImageError("context_directory_not_private")
        return info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid


def _assert_directory(path, expected, *, private=False):
    if _directory_identity(path, private=private) != expected:
        raise ImageError("context_directory_changed")


def _metadata(root, name):
    before = _current_file(root, name)
    data = read_regular(root, name, _METADATA_LIMIT)
    if _current_file(root, name) != before:
        raise ImageError("context_file_changed")
    return data


def _write_all(fd, content):
    remaining = memoryview(content)
    while remaining:
        count = os.write(fd, remaining)
        if count <= 0:
            raise ImageError("context_write_failed")
        remaining = remaining[count:]


def _inspect_package(root, package, expected=None, output_fd=None):
    """One bounded stream; the descriptor and current pathname must still agree."""
    with file_descriptor(root, package["file"]) as (fd, before):
        identity = _file_identity(before)
        if (before.st_size != package["size"] or before.st_size > MAX_FILE_BYTES
                or expected is not None and identity != expected):
            raise ImageError("package_identity_changed")
        digest, total, header = hashlib.sha256(), 0, b""
        while True:
            chunk = os.read(fd, min(_CHUNK, package["size"] + 1 - total))
            if not chunk:
                break
            total += len(chunk)
            if total > package["size"]:
                raise ImageError("package_size_mismatch")
            header = (header + chunk[:8])[:8]
            digest.update(chunk)
            if output_fd is not None:
                _write_all(output_fd, chunk)
        if (total != package["size"] or digest.hexdigest() != package["sha256"]
                or header != b"!<arch>\n"):
            raise ImageError("package_bytes_rejected")
        if _file_identity(os.fstat(fd)) != identity or _current_file(root, package["file"]) != identity:
            raise ImageError("package_identity_changed")
        return identity


def _copy_package(source_root, package, identity, destination):
    with open_directory(destination) as parent_fd:
        fd = os.open(package["file"], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                     0o600, dir_fd=parent_fd)
        try:
            os.fchmod(fd, 0o600)
            _inspect_package(source_root, package, identity, fd)
            os.fsync(fd)
            copied = _file_identity(os.fstat(fd))
            if _current_file(destination, package["file"]) != copied:
                raise ImageError("context_file_changed")
        finally:
            os.close(fd)


def _names(fd, limit):
    names = set()
    with os.scandir(fd) as entries:
        for entry in entries:
            if len(names) == limit:
                raise ImageError("context_inventory_mismatch")
            names.add(entry.name)
    return names


def _private_file(root, name):
    with file_descriptor(root, name) as (_, info):
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o600:
            raise ImageError("context_file_not_private")
        return _file_identity(info)


def _inventory(destination, lock, *, complete):
    root_files = _ROOT_FILES | ({"receipt.json"} if complete else set())
    _directory_identity(destination, private=True)
    with open_directory(destination) as fd:
        if _names(fd, len(root_files) + 1) != root_files | {"packages"}:
            raise ImageError("context_inventory_mismatch")
    proofs = {name: _private_file(destination, name) for name in root_files}
    package_root = destination / "packages"
    _directory_identity(package_root, private=True)
    files = {package["file"] for package in lock["packages"]}
    with open_directory(package_root) as fd:
        if _names(fd, len(files)) != files:
            raise ImageError("context_inventory_mismatch")
    proofs.update({"packages/" + name: _private_file(package_root, name) for name in files})
    return proofs


def _receipt(lock, canonical, recipe, checksums):
    return {
        "schema_version": 1, "kind": "isthmus-base-build-context", "status": "complete",
        "lock_sha256": hashlib.sha256(canonical).hexdigest(),
        "dockerfile_sha256": hashlib.sha256(recipe).hexdigest(),
        "checksums_sha256": hashlib.sha256(checksums).hexdigest(),
        "artifact_count": len(lock["packages"]),
        "total_bytes": sum(package["size"] for package in lock["packages"]),
    }


def _verify(destination, *, complete):
    root_identity = _directory_identity(destination, private=True)
    lock_identity = _private_file(destination, "artifacts.lock.json")
    raw_lock = _metadata(destination, "artifacts.lock.json")
    lock = validate_lock(load_json_bytes(raw_lock))
    canonical = json_bytes(lock)
    if raw_lock != canonical:
        raise ImageError("context_lock_not_canonical")
    recipe, checksums = render_recipe(lock), render_checksums(lock)
    expected_receipt = _receipt(lock, canonical, recipe, checksums)
    if expected_receipt["total_bytes"] > MAX_TOTAL_BYTES:
        raise ImageError("package_total_limit")
    proofs = _inventory(destination, lock, complete=complete)
    if proofs["artifacts.lock.json"] != lock_identity:
        raise ImageError("context_lock_changed")
    package_root = destination / "packages"
    package_identity = _directory_identity(package_root, private=True)
    if (_metadata(destination, "Dockerfile") != recipe
            or _metadata(destination, "packages.sha256") != checksums):
        raise ImageError("context_recipe_mismatch")
    for package in lock["packages"]:
        _inspect_package(package_root, package, proofs["packages/" + package["file"]])
    if complete and _metadata(destination, "receipt.json") != json_bytes(expected_receipt):
        # Byte-exact canonical metadata also rejects extra fields and bools
        # masquerading as integers; a receipt is never a signature attestation.
        raise ImageError("context_receipt_mismatch")
    if _inventory(destination, lock, complete=complete) != proofs:
        raise ImageError("context_file_changed")
    _assert_directory(package_root, package_identity, private=True)
    _assert_directory(destination, root_identity, private=True)
    return expected_receipt


def _report(receipt):
    return {"status": "base_context_verified", "image_built": False,
            "execution_permitted": False, "artifact_count": receipt["artifact_count"],
            "total_bytes": receipt["total_bytes"]}


def stage_context(lock_path, source_root, destination) -> dict:
    """Copy reviewed bytes; leave private output on error, never auto-clean it.

    Final I/O failure can leave a receipt on disk. Its existence is not success:
    the caller needs a successful independent verify_context before later use.
    """
    try:
        lock_path = absolute_path(lock_path, required=True)
        source_root = absolute_path(source_root, required=True)
        destination = absolute_path(destination, required=True)
        lock_identity = _current_file(lock_path.parent, lock_path.name)
        lock = load_lock(lock_path)
        source_identity = _directory_identity(source_root)
        proofs = {package["file"]: _inspect_package(source_root, package) for package in lock["packages"]}
        canonical = json_bytes(lock)
        recipe, checksums = render_recipe(lock), render_checksums(lock)
        expected_receipt = _receipt(lock, canonical, recipe, checksums)
        if any(len(data) > _METADATA_LIMIT for data in (canonical, recipe, checksums)):
            raise ImageError("context_metadata_limit")
        if _current_file(lock_path.parent, lock_path.name) != lock_identity:
            raise ImageError("context_lock_changed")
        _assert_directory(source_root, source_identity)
        create_private_destination(destination, (source_root,))
        root_identity = _directory_identity(destination, private=True)
        with open_directory(destination) as fd:
            os.mkdir("packages", 0o700, dir_fd=fd)
        package_root = destination / "packages"
        with open_directory(package_root) as fd:
            os.fchmod(fd, 0o700)
        package_identity = _directory_identity(package_root, private=True)
        for package in lock["packages"]:
            _assert_directory(destination, root_identity, private=True)
            _assert_directory(package_root, package_identity, private=True)
            _copy_package(source_root, package, proofs[package["file"]], package_root)
        for name, data in (("Dockerfile", recipe), ("packages.sha256", checksums), ("artifacts.lock.json", canonical)):
            _assert_directory(destination, root_identity, private=True)
            write_exclusive(destination, name, data)
        # The source paths must still designate exactly what was reviewed, not
        # same-byte replacements or files swapped between preflight and copying.
        for package in lock["packages"]:
            if _current_file(source_root, package["file"]) != proofs[package["file"]]:
                raise ImageError("package_identity_changed")
        if _current_file(lock_path.parent, lock_path.name) != lock_identity:
            raise ImageError("context_lock_changed")
        _assert_directory(source_root, source_identity)
        _assert_directory(destination, root_identity, private=True)
        _assert_directory(package_root, package_identity, private=True)
        receipt = _verify(destination, complete=False)
        if receipt != expected_receipt:
            raise ImageError("context_inputs_changed")
        write_exclusive(destination, "receipt.json", json_bytes(receipt))
        # A receipt file alone never implies success: final verification is
        # mandatory, including on later independent uses of the context.
        final_receipt = _verify(destination, complete=True)
        if final_receipt != expected_receipt:
            raise ImageError("context_inputs_changed")
        return _report(final_receipt)
    except ImageError:
        raise
    except (EvidenceError, OSError, TypeError, ValueError):
        raise ImageError("context_stage_failed") from None


def verify_context(destination) -> dict:
    try:
        destination = absolute_path(destination, required=True)
        return _report(_verify(destination, complete=True))
    except ImageError:
        raise
    except (EvidenceError, OSError, TypeError, ValueError):
        raise ImageError("context_verification_failed") from None
