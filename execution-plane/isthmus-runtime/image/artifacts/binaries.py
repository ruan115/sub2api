"""Offline staging of reviewed Bun/Claude bytes, never download or execution.

The lock binds metadata, not a publisher signature. Callers retain and verify
official provenance separately; no PGP/authentication claim is emitted here.
ELF checks are a minimal format/architecture gate, not full linkability analysis.
Failed staging can leave a partial file; never infer success from its existence,
overwrite it, or automatically clean it up. Use a fresh private destination.
"""
from contextlib import ExitStack
import hashlib
import os
import re
import stat
import struct
import zipfile
import zlib

from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import absolute_path, file_descriptor, open_directory, relative_path


MAX_INPUT_BYTES = 256 * 1024 * 1024
MAX_BINARY_BYTES = 256 * 1024 * 1024
_CHUNK = 1024 * 1024
# Reviewed CLI releases. A lock still pins one version at a time: validate_lock
# requires exactly six artifacts, which is two platforms of one CLI version plus
# the two Bun builds. Contract differential acquires each version separately so
# every one keeps its own signed manifest verification.
CLAUDE_VERSIONS = ("2.1.258", "2.1.280")
_FIELDS = {"name", "role", "version", "platform", "format", "file", "source_url", "size", "sha256", "member"}
_SHA = re.compile(r"[a-f0-9]{64}\Z")
_DEST = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}\Z")


class ArtifactError(ValueError):
    """Only fixed reasons, with no input paths, package bytes or raw exceptions."""


def _spec(value):
    if type(value) is not dict or set(value) != _FIELDS:
        raise ArtifactError("invalid_binary_spec")
    if any(type(value[key]) is not str for key in _FIELDS - {"size", "member"}):
        raise ArtifactError("invalid_binary_spec")
    if (type(value["size"]) is not int or not 64 <= value["size"] <= MAX_INPUT_BYTES
            or not _SHA.fullmatch(value["sha256"])
            or value["platform"] not in ("linux/amd64", "linux/arm64")):
        raise ArtifactError("invalid_binary_spec")
    name, version, platform = value["name"], value["version"], value["platform"]
    if name == "bun" and version in ("1.4.2", "1.3.9"):
        arch = "x64" if platform == "linux/amd64" else "aarch64"
        expected = {"role": "candidate" if version == "1.4.2" else "control", "format": "zip",
                    "file": f"bun-{version}-linux-{arch}.zip", "member": f"bun-linux-{arch}/bun",
                    "source_url": f"https://github.com/oven-sh/bun/releases/download/bun-v{version}/bun-linux-{arch}.zip"}
    elif name == "claude" and version in CLAUDE_VERSIONS:
        arch = "x64" if platform == "linux/amd64" else "arm64"
        expected = {"role": "cli", "format": "elf", "member": None,
                    "file": f"claude-{version}-linux-{arch}",
                    "source_url": f"https://downloads.claude.ai/claude-code-releases/{version}/linux-{arch}/claude"}
    else:
        raise ArtifactError("unsupported_binary_identity")
    if any(value[key] != expected[key] for key in expected):
        raise ArtifactError("binary_source_or_identity_mismatch")
    return dict(value)


def validate_lock(document: dict) -> dict:
    """Validate/copy the six reviewed records; hashes are caller-trusted input."""
    if (type(document) is not dict or set(document) != {"schema_version", "kind", "artifacts"}
            or type(document["schema_version"]) is not int or document["schema_version"] != 1
            or type(document["kind"]) is not str or document["kind"] != "isthmus-toolchain-artifacts"
            or type(document["artifacts"]) is not list or len(document["artifacts"]) != 6):
        raise ArtifactError("invalid_toolchain_lock")
    entries = [_spec(value) for value in document["artifacts"]]
    identities = {(item["name"], item["version"], item["platform"]) for item in entries}
    if len(identities) != 6:
        raise ArtifactError("duplicate_binary_identity")
    return {"schema_version": 1, "kind": "isthmus-toolchain-artifacts",
            "artifacts": sorted(entries, key=lambda item: item["file"])}


def _identity(info):
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ArtifactError("binary_file_type_rejected")
    return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid,
            info.st_nlink, info.st_size, info.st_mtime_ns, info.st_ctime_ns)


def _current(path):
    with file_descriptor(path.parent, path.name) as (_, info):
        return _identity(info)


def _hash_fd(fd, limit):
    os.lseek(fd, 0, os.SEEK_SET)
    digest, total = hashlib.sha256(), 0
    while True:
        part = os.read(fd, min(_CHUNK, limit + 1 - total))
        if not part:
            break
        total += len(part)
        if total > limit:
            raise ArtifactError("binary_size_mismatch")
        digest.update(part)
    return total, digest.hexdigest()


def _check_input(fd, path, identity, metadata, *, hash_bytes=False):
    if (_identity(os.fstat(fd)) != identity or _current(path) != identity
            or identity[6] != metadata["size"]):
        raise ArtifactError("binary_input_changed")
    if hash_bytes:
        if _hash_fd(fd, metadata["size"]) != (metadata["size"], metadata["sha256"]):
            raise ArtifactError("binary_outer_digest_mismatch")
        _check_input(fd, path, identity, metadata)


def _parent(path):
    with open_directory(path) as fd:
        info = os.fstat(fd)
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) != 0o700:
            raise ArtifactError("binary_destination_not_private")
        return info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_gid


def _zip_member(reader, size, metadata):
    # Bound metadata BEFORE ZipFile allocates its central-directory objects.
    reader.seek(max(0, size - 65557))
    tail = reader.read(65557)
    index = tail.rfind(b"PK\x05\x06")
    if index < 0 or index + 22 > len(tail):
        raise ArtifactError("binary_zip_directory_rejected")
    (_, disk, cd_disk, disk_entries, entries, cd_size, cd_offset, comment) = struct.unpack_from("<4s4H2IH", tail, index)
    end_offset = size - len(tail) + index
    if (disk != 0 or cd_disk != 0 or disk_entries != entries or entries not in (1, 2)
            or not 1 <= cd_size <= 8192 or cd_offset + cd_size != end_offset
            or index + 22 + comment != len(tail)):
        raise ArtifactError("binary_zip_directory_rejected")
    # ZipFile lets an immediately preceding ZIP64 locator override these
    # bounded legacy fields. Reject it before the parser can allocate a larger
    # directory. Read its fixed position separately: with a 65535-byte comment
    # the locator lies just outside the EOCD tail buffer above.
    if end_offset >= 20:
        reader.seek(end_offset - 20)
        if reader.read(4) == b"PK\x06\x07":
            raise ArtifactError("binary_zip_directory_rejected")
    zipped = zipfile.ZipFile(reader, "r")
    try:
        members = zipped.infolist()
        parent = metadata["member"].rsplit("/", 1)[0] + "/"
        if len(members) != entries or len({info.filename for info in members}) != entries:
            raise ArtifactError("binary_zip_members_rejected")
        selected = None
        for info in members:
            if (info.orig_filename != info.filename or info.filename not in (metadata["member"], parent)
                    or info.flag_bits & ~(0x0006 | 0x0008 | 0x0800)
                    or info.compress_type not in (zipfile.ZIP_STORED, zipfile.ZIP_DEFLATED)
                    or info.compress_size > size or not 0 <= info.header_offset < cd_offset):
                raise ArtifactError("binary_zip_members_rejected")
            kind = stat.S_IFMT(info.external_attr >> 16)
            if info.filename == parent:
                if not info.is_dir() or info.file_size != 0 or kind not in (0, stat.S_IFDIR):
                    raise ArtifactError("binary_zip_members_rejected")
            else:
                if (info.is_dir() or kind not in (0, stat.S_IFREG)
                        or not 64 <= info.file_size <= MAX_BINARY_BYTES):
                    raise ArtifactError("binary_zip_members_rejected")
                selected = info
        if selected is None:
            raise ArtifactError("binary_zip_members_rejected")
        return zipped, selected
    except BaseException:
        zipped.close()
        raise


def _elf_header(header, platform):
    expected_machine = 62 if platform == "linux/amd64" else 183
    if (len(header) != 64 or header[:7] != b"\x7fELF\x02\x01\x01" or header[7] not in (0, 3)
            or struct.unpack_from("<H", header, 16)[0] not in (2, 3)
            or struct.unpack_from("<H", header, 18)[0] != expected_machine
            or struct.unpack_from("<I", header, 20)[0] != 1
            or struct.unpack_from("<H", header, 52)[0] != 64):
        raise ArtifactError("binary_elf_rejected")


def _write_all(fd, content):
    remaining = memoryview(content)
    while remaining:
        written = os.write(fd, remaining)
        if written <= 0:
            raise ArtifactError("binary_write_failed")
        remaining = remaining[written:]


def _copy_binary(stream, header, size, fd):
    total, digest = len(header), hashlib.sha256(header)
    _write_all(fd, header)
    while True:
        part = stream.read(min(_CHUNK, size + 1 - total))
        if not part:
            break
        total += len(part)
        if total > size or total > MAX_BINARY_BYTES:
            raise ArtifactError("binary_expansion_limit")
        digest.update(part)
        _write_all(fd, part)
    if total != size:
        raise ArtifactError("binary_size_mismatch")
    return total, digest.hexdigest()


def stage_binary(input_path, spec, dest) -> dict:
    """Publish only a new 0755 binary in an existing private, non-Git parent.

    No receipt is written to disk. A successful returned receipt is necessary;
    errors may leave a partial 0600 file, or a 0755 file if final verification
    or I/O fails. Its existence or mode must not be treated as success.
    This does not authorize executing the staged file or promote an image gate.
    """
    try:
        metadata = _spec(spec)
        input_path = absolute_path(input_path, required=True)
        dest = absolute_path(dest, required=True)
        if not _DEST.fullmatch(dest.name):
            raise ArtifactError("binary_destination_rejected")
        relative_path(input_path.name)
        parent_identity = _parent(dest.parent)
        if any(os.path.lexists(ancestor / ".git") for ancestor in (dest.parent, *dest.parent.parents)):
            raise ArtifactError("binary_destination_in_git")
        with ExitStack() as stack:
            source_fd, before = stack.enter_context(file_descriptor(input_path.parent, input_path.name))
            identity = _identity(before)
            _check_input(source_fd, input_path, identity, metadata, hash_bytes=True)
            reader = stack.enter_context(os.fdopen(os.dup(source_fd), "rb"))
            reader.seek(0)
            if metadata["format"] == "zip":
                zipped, member = _zip_member(reader, metadata["size"], metadata)
                stack.enter_context(zipped)
                stream = stack.enter_context(zipped.open(member, "r"))
                size = member.file_size
            else:
                stream, size = reader, metadata["size"]
                if size > MAX_BINARY_BYTES:
                    raise ArtifactError("binary_expansion_limit")
            header = stream.read(64)
            _elf_header(header, metadata["platform"])
            _check_input(source_fd, input_path, identity, metadata)
            if _parent(dest.parent) != parent_identity:
                raise ArtifactError("binary_destination_changed")
            parent_fd = stack.enter_context(open_directory(dest.parent))
            fd = os.open(dest.name, os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600, dir_fd=parent_fd)
            try:
                os.fchmod(fd, 0o600)
                result = _copy_binary(stream, header, size, fd)
                os.fsync(fd)
                _check_input(source_fd, input_path, identity, metadata, hash_bytes=True)
                output_identity = _identity(os.fstat(fd))
                if (_hash_fd(fd, size) != result or _identity(os.fstat(fd)) != output_identity
                        or _current(dest) != output_identity or _parent(dest.parent) != parent_identity):
                    raise ArtifactError("binary_output_changed")
                os.fchmod(fd, 0o755)
                os.fsync(fd)
                # chmod is the last intentional mutation. Freeze its resulting
                # identity, then re-read bytes: merely comparing two fresh stats
                # would accept a write that occurred during chmod/fsync.
                published_identity = _identity(os.fstat(fd))
                if (stat.S_IMODE(published_identity[2]) != 0o755
                        or _hash_fd(fd, size) != result
                        or _identity(os.fstat(fd)) != published_identity
                        or _current(dest) != published_identity
                        or _parent(dest.parent) != parent_identity):
                    raise ArtifactError("binary_output_changed")
            finally:
                os.close(fd)
        return {"status": "binary_staged", "name": metadata["name"], "role": metadata["role"],
                "version": metadata["version"], "platform": metadata["platform"],
                "size": result[0], "sha256": result[1],
                "source_size": metadata["size"], "source_sha256": metadata["sha256"],
                "image_built": False, "execution_permitted": False}
    except ArtifactError:
        raise
    except (EvidenceError, OSError, TypeError, ValueError, RuntimeError,
            zipfile.BadZipFile, struct.error, zlib.error):
        raise ArtifactError("binary_stage_failed") from None
