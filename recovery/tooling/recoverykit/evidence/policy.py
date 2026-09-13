"""Conservative text-evidence allowlist and recognisable-secret screening.

This is a deterministic rejection policy, not a claim that arbitrary source code
can be proven secret-free. Manifests still require an explicit reviewed allowlist.
"""

from __future__ import annotations

from pathlib import PurePosixPath
import re

from .errors import EvidenceError
from .filesystem import relative_path

MAX_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_BYTES = 64 * 1024 * 1024
MAX_METADATA_BYTES = 1024 * 1024
MAX_ENTRIES = 4096

TEXT_SUFFIXES = {".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".proto", ".sh",
                 ".apparmor", ".json", ".md", ".txt", ".css", ".html", ".svg",
                 ".yml", ".yaml", ".toml", ".lock", ".sql", ".xml", ".go",
                 ".rs", ".py", ".ini", ".conf"}
FORBIDDEN_SUFFIXES = {".key", ".pem", ".crt", ".p12", ".pfx", ".p7b", ".der",
                      ".zip", ".tar", ".gz", ".tgz", ".bz2", ".xz", ".7z",
                      ".rar", ".pkg", ".log", ".db", ".sqlite", ".sqlite3",
                      ".dump", ".rdb"}
FORBIDDEN_COMPONENTS = {".git", ".ssh", ".aws", ".gnupg", ".kube", "pg_wal",
                        "pg_tblspc", "grpcs-certs", "private_keys"}
FORBIDDEN_NAMES = {"credentials.json", ".credentials.json", "credentials", ".netrc",
                   ".npmrc", ".pypirc", "id_rsa", "id_ed25519", "id_ecdsa",
                   "authorized_keys", "known_hosts", "pg_filenode.map", "pg_control"}
SECRET_PATTERNS = (
    re.compile(rb"-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----"
               rb"(?:[\r\n\t ]|\\[rn])+[A-Za-z0-9+/]{32,}"),
    re.compile(rb"\bsk-(?:ant-(?:api03|oat01|sid01)-)?[A-Za-z0-9_-]{32,}"),
    re.compile(rb"\b(?:ghp_|github_pat_|gho_|ghu_|ghs_|ghr_)[A-Za-z0-9_]{30,}"),
    re.compile(rb"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(rb"\b(?:https?|socks5h?|postgres(?:ql)?|mysql|redis)://"
               rb"[^\s/<>\"'`:@]{1,128}:[^\s/<>\"'`@]{1,128}@", re.I),
)


def check_sensitive_path(name: str) -> None:
    parts = relative_path(name).split("/")
    lowered = [part.casefold() for part in parts]
    if any(part in FORBIDDEN_COMPONENTS for part in lowered):
        raise EvidenceError("sensitive path is forbidden")
    base = lowered[-1]
    if (base.startswith(".env") or base in FORBIDDEN_NAMES
            or PurePosixPath(base).suffix in FORBIDDEN_SUFFIXES):
        raise EvidenceError("sensitive or opaque file is forbidden")


def check_evidence_path(name: str) -> None:
    check_sensitive_path(name)
    base = PurePosixPath(name).name
    if (PurePosixPath(base).suffix.casefold() not in TEXT_SUFFIXES
            and not base.startswith("Dockerfile") and base not in {"Makefile", "Caddyfile"}):
        raise EvidenceError("file is not in the text-evidence type allowlist")


def check_content(content: bytes, *, text_only: bool) -> None:
    if any(pattern.search(content) for pattern in SECRET_PATTERNS):
        raise EvidenceError("recognisable secret material is forbidden")
    if text_only:
        if b"\x00" in content:
            raise EvidenceError("binary evidence is forbidden")
        try:
            content.decode("utf-8")
        except UnicodeError as exc:
            raise EvidenceError("text evidence must be UTF-8") from exc
