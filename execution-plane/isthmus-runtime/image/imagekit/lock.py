"""Strict reviewed-input contract, not an APT signature or dependency verifier."""
import re
from urllib.parse import urlsplit

from recoverykit.evidence.filesystem import absolute_path, load_json_bytes, read_regular, relative_path
from recoverykit.evidence.policy import check_content


MAX_FILE_BYTES = 64 * 1024 * 1024
MAX_TOTAL_BYTES = 256 * 1024 * 1024
MAX_METADATA_BYTES = 256 * 1024
MAX_PACKAGES = 128
REQUIRED_PACKAGES = frozenset({"ca-certificates", "passwd", "procps", "util-linux"})
_ROOT_FIELDS = {"schema_version", "kind", "platform", "base_image", "packages"}
_PACKAGE_FIELDS = {"file", "name", "version", "architecture", "size", "sha256", "source_url"}
_DIGEST = re.compile(r"[a-f0-9]{64}\Z")
_BASE = re.compile(r"docker\.io/library/debian@sha256:[a-f0-9]{64}\Z")
_NAME = re.compile(r"[a-z0-9][a-z0-9+.-]{0,63}\Z")
_VERSION = re.compile(r"(?:[0-9]{1,9}:)?[0-9][A-Za-z0-9.+~\-]{0,127}\Z")
_SNAPSHOT = re.compile(r"/archive/debian(?:-security)?/[0-9]{8}T[0-9]{6}Z/(.+)\Z")


class ImageError(ValueError):
    """Fixed reasons only; CLI never emits untrusted input or exceptions."""


def _source(value, filename):
    if (type(value) is not str or not 1 <= len(value) <= 512 or not value.isascii()
            or any(ord(c) <= 32 or ord(c) == 127 for c in value)
            or any(marker in value for marker in ("%", "\\", "?", "#"))):
        raise ImageError("package_source_rejected")
    parsed = urlsplit(value)
    if parsed.scheme != "https" or parsed.query or parsed.fragment:
        raise ImageError("package_source_rejected")
    if parsed.netloc == "deb.debian.org" and parsed.path.startswith("/debian/"):
        path = parsed.path[len("/debian/"):]
    elif parsed.netloc == "security.debian.org" and parsed.path.startswith("/debian-security/"):
        path = parsed.path[len("/debian-security/"):]
    elif parsed.netloc == "snapshot.debian.org" and _SNAPSHOT.fullmatch(parsed.path):
        path = _SNAPSHOT.fullmatch(parsed.path).group(1)
    else:
        raise ImageError("package_source_rejected")
    relative_path(path)
    if (not re.fullmatch(r"pool/(?:updates/)?main/[A-Za-z0-9+./_~\-]+", path)
            or path.rsplit("/", 1)[-1] != filename):
        raise ImageError("package_source_rejected")


def validate_lock(document):
    if type(document) is not dict or set(document) != _ROOT_FIELDS:
        raise ImageError("invalid_lock_fields")
    if type(document["schema_version"]) is not int or document["schema_version"] != 1:
        raise ImageError("invalid_lock_schema")
    if (type(document["kind"]) is not str or document["kind"] != "isthmus-base-build-inputs"
            or type(document["platform"]) is not str or document["platform"] not in ("linux/amd64", "linux/arm64")
            or type(document["base_image"]) is not str or not _BASE.fullmatch(document["base_image"])):
        raise ImageError("invalid_base_identity")
    packages = document["packages"]
    if type(packages) is not list or not 1 <= len(packages) <= MAX_PACKAGES:
        raise ImageError("invalid_package_count")
    names, files, result, total = set(), set(), [], 0
    architecture = document["platform"].split("/", 1)[1]
    for package in packages:
        if type(package) is not dict or set(package) != _PACKAGE_FIELDS:
            raise ImageError("invalid_package_fields")
        name, version, filename = package["name"], package["version"], package["file"]
        if (type(name) is not str or not _NAME.fullmatch(name)
                or type(version) is not str or not _VERSION.fullmatch(version)
                or package["architecture"] not in (architecture, "all")
                or type(filename) is not str
                or filename != f"{name}_{version.split(':')[-1]}_{package['architecture']}.deb"):
            raise ImageError("invalid_package_identity")
        if name in names or filename.casefold() in files:
            raise ImageError("duplicate_package")
        # This scope deliberately does not restore the old setcap primitive.
        # Other packages can contain maintainer scripts: reviewed provenance and
        # an isolated builder remain mandatory, not inferred from this denylist.
        if name in {"libcap2-bin", "sudo", "openssh-server", "docker.io", "containerd", "runc"}:
            raise ImageError("package_outside_base_scope")
        size, digest = package["size"], package["sha256"]
        if (type(size) is not int or not 8 <= size <= MAX_FILE_BYTES
                or type(digest) is not str or not _DIGEST.fullmatch(digest)):
            raise ImageError("invalid_package_digest_or_size")
        _source(package["source_url"], filename)
        check_content(package["source_url"].encode(), text_only=True)
        total += size
        if total > MAX_TOTAL_BYTES:
            raise ImageError("package_total_limit")
        names.add(name)
        files.add(filename.casefold())
        result.append(dict(package))
    if not REQUIRED_PACKAGES <= names:
        raise ImageError("required_package_missing")
    return {**document, "packages": sorted(result, key=lambda item: item["file"])}


def load_lock(path):
    path = absolute_path(path, required=True)
    data = read_regular(path.parent, path.name, MAX_METADATA_BYTES)
    check_content(data, text_only=True)
    return validate_lock(load_json_bytes(data))
