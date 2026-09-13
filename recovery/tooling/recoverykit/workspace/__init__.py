"""Public, offline WIP capture and verification API. No restore operations."""

from .errors import WorkspaceError
from .snapshot import snapshot_workspace, verify_snapshot

__all__ = ["WorkspaceError", "snapshot_workspace", "verify_snapshot"]
