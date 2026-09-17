"""Bounded metadata agreement, not archive authentication or dependency solving.

The caller must obtain the exact-version record from an authenticated APT index,
read the control fields from the same downloaded bytes that were measured, and
retain that provenance separately. This module does no I/O or package execution.
After collecting entries, call imagekit.lock.validate_lock for target architecture,
core-package, duplicate-package and aggregate-budget validation.
"""
import re

from recoverykit.evidence.errors import EvidenceError
from imagekit.lock import ImageError, MAX_FILE_BYTES, _DIGEST, _NAME, _VERSION, _source


_ORIGINS = frozenset({"https://deb.debian.org/debian/",
                      "https://security.debian.org/debian-security/"})
_IDENTITY_FIELDS = frozenset({"package", "version", "architecture"})
_INDEX_FIELDS = _IDENTITY_FIELDS | {"filename", "size", "sha256"}
_FIELD = re.compile(r"([A-Za-z0-9][A-Za-z0-9-]{0,63}):(.*)\Z")
_SIZE = re.compile(r"[1-9][0-9]{0,7}\Z")
_OUTSIDE_SCOPE = frozenset({"libcap2-bin", "sudo", "openssh-server", "docker.io", "containerd", "runc"})


def _stanza(text, limit, required, *, control=False):
    if type(text) is not str or not text or len(text) > limit:
        raise ImageError("package_metadata_rejected")
    try:
        if len(text.encode("utf-8")) > limit:
            raise ImageError("package_metadata_rejected")
    except UnicodeError:
        raise ImageError("package_metadata_rejected") from None
    text = text.replace("\r\n", "\n")
    if any((ord(c) < 32 and c not in "\t\n") or ord(c) == 127 for c in text):
        raise ImageError("package_metadata_rejected")
    fields, current, ended = {}, None, False
    for line in text.split("\n"):
        if len(line.encode("utf-8")) > 16 * 1024:
            raise ImageError("package_metadata_rejected")
        if not line:
            if not fields:
                raise ImageError("package_metadata_rejected")
            ended = True
            continue
        if ended:
            raise ImageError("package_metadata_rejected")
        if line[0] in " \t":
            # Unknown descriptive fields can wrap; authorization/identity fields cannot.
            if current is None or current in required:
                raise ImageError("package_metadata_rejected")
            continue
        match = _FIELD.fullmatch(line)
        if match is None:
            raise ImageError("package_metadata_rejected")
        key, value = match.group(1).lower(), match.group(2).strip(" \t")
        if key in fields or len(fields) >= 128:
            raise ImageError("package_metadata_rejected")
        fields[key], current = value, key
    if not required <= fields.keys() or (control and set(fields) != required):
        raise ImageError("package_metadata_rejected")
    return fields


def build_package(record: str, control: str, measured_size: int,
                  measured_sha256: str, origin: str) -> dict:
    """Compare one apt-cache stanza and labelled dpkg-deb identity fields.

    `control` is the labelled output of `dpkg-deb --field archive.deb Package
    Version Architecture`. `record` must be exactly one full exact-version stanza,
    not the merged output of multiple repositories/versions. `origin` is an exact
    approved HTTPS archive root including its trailing slash, without redirects.
    The return value is one lock entry; it proves agreement only, not provenance.
    """
    if type(origin) is not str or origin not in _ORIGINS:
        raise ImageError("package_origin_rejected")
    if (type(measured_size) is not int or not 8 <= measured_size <= MAX_FILE_BYTES
            or type(measured_sha256) is not str or not _DIGEST.fullmatch(measured_sha256)):
        raise ImageError("package_measurement_rejected")
    index = _stanza(record, 256 * 1024, _INDEX_FIELDS)
    identity = _stanza(control, 4 * 1024, _IDENTITY_FIELDS, control=True)
    if any(index[key] != identity[key] for key in _IDENTITY_FIELDS):
        raise ImageError("package_identity_mismatch")
    name, version, architecture = (identity[key] for key in ("package", "version", "architecture"))
    if (not _NAME.fullmatch(name) or not _VERSION.fullmatch(version)
            or architecture not in ("all", "amd64", "arm64") or name in _OUTSIDE_SCOPE):
        raise ImageError("package_identity_rejected")
    if (not _SIZE.fullmatch(index["size"]) or int(index["size"]) != measured_size
            or index["sha256"] != measured_sha256):
        raise ImageError("package_measurement_mismatch")
    filename = f"{name}_{version.split(':')[-1]}_{architecture}.deb"
    source = origin + index["filename"]
    # Share the existing flat-filename, main-pool, URL and text-evidence policy.
    try:
        _source(source, filename)
    except (ImageError, EvidenceError, ValueError):
        raise ImageError("package_source_rejected") from None
    return {"file": filename, "name": name, "version": version, "architecture": architecture,
            "size": measured_size, "sha256": measured_sha256, "source_url": source}
