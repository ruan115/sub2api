"""No ambient Docker routing, credentials, or shared global socket selection."""
from dataclasses import dataclass
import os
from pathlib import Path
import re
import stat


class LabError(ValueError):
    """Fixed codes only; no input values or daemon response content."""


_AMBIENT = frozenset({
    "docker_host", "docker_context", "docker_config", "docker_tls_verify",
    "docker_cert_path", "docker_api_version", "docker_auth_config",
    "http_proxy", "https_proxy", "all_proxy", "no_proxy", "ssh_auth_sock",
})
_IDENTIFIER = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}\Z")


@dataclass(frozen=True)
class LabConfig:
    socket_path: Path
    engine_id: str
    engine_name: str
    architecture: str

    def validate_inputs(self, environment):
        if any(key.lower() in _AMBIENT and value for key, value in environment.items()):
            raise LabError("ambient_routing_forbidden")
        if not isinstance(self.socket_path, Path):
            raise LabError("invalid_socket_path")
        raw = str(self.socket_path)
        if (not self.socket_path.is_absolute() or ".." in self.socket_path.parts
                or "\\" in raw or any(ord(c) < 32 or ord(c) == 127 for c in raw)
                or raw in {"/var/run/docker.sock", "/run/docker.sock"}
                or len(os.fsencode(raw)) > 100):
            raise LabError("invalid_socket_path")
        if (not isinstance(self.engine_id, str) or not _IDENTIFIER.fullmatch(self.engine_id)
                or not isinstance(self.engine_name, str) or not _IDENTIFIER.fullmatch(self.engine_name)
                or self.engine_name.lower() in {"default", "docker-desktop", "colima"}
                or self.architecture not in {"amd64", "arm64"}):
            raise LabError("invalid_expected_identity")

    def socket_identity(self):
        self.validate_inputs({})
        fd = None
        uid = os.getuid()
        try:
            flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
            fd = os.open(self.socket_path.anchor, flags)
            for component in self.socket_path.parts[1:-1]:
                next_fd = os.open(component, flags, dir_fd=fd)
                os.close(fd)
                fd = next_fd
                info = os.fstat(fd)
                # Root-owned sticky ancestors (e.g. /private/tmp) are allowed;
                # the direct parent below must still be user-owned 0700.
                writable = info.st_mode & 0o022
                sticky_root = info.st_uid == 0 and bool(info.st_mode & stat.S_ISVTX)
                if info.st_uid not in (0, uid) or (writable and not sticky_root):
                    raise LabError("untrusted_socket_ancestor")
            parent = os.fstat(fd)
            if parent.st_uid != uid or stat.S_IMODE(parent.st_mode) != 0o700:
                raise LabError("socket_parent_not_private")
            info = os.stat(self.socket_path.name, dir_fd=fd, follow_symlinks=False)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != uid:
                raise LabError("invalid_socket_owner_or_type")
            return (info.st_dev, info.st_ino, info.st_uid, info.st_mode,
                    info.st_mtime_ns, info.st_ctime_ns, parent.st_dev, parent.st_ino)
        except OSError:
            raise LabError("socket_unavailable") from None
        finally:
            if fd is not None:
                os.close(fd)
