"""Read-only Git capture with explicit binary patches and no optional index locks."""

from __future__ import annotations

import os
from pathlib import Path
import re

from ..evidence.filesystem import absolute_path, open_directory, relative_path
from .errors import WorkspaceError
from .process import ProcessError, run_bounded

MAX_GIT_BYTES = 64 * 1024 * 1024


def git(repo: Path, *args: str, optional: bool = False) -> bytes:
    environment = os.environ.copy()
    # Do not let a caller's Git routing environment redirect a requested snapshot.
    for name in list(environment):
        if name.startswith("GIT_"):
            environment.pop(name)
    environment.update(GIT_OPTIONAL_LOCKS="0", GIT_TERMINAL_PROMPT="0", LC_ALL="C")
    command = ["git", "--no-pager", "-c", "color.ui=false", "-c", "core.quotePath=true",
               "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
               "-c", "core.untrackedCache=false", "-C", str(repo), *args]
    try:
        result = run_bounded(command, environment=environment,
                             stdout_limit=MAX_GIT_BYTES, timeout_seconds=60)
    except ProcessError as exc:
        if str(exc) == "output_limit":
            raise WorkspaceError("Git output exceeds the snapshot size limit") from exc
        raise WorkspaceError("read-only Git command failed or timed out") from exc
    if result.returncode != 0:
        if optional and result.returncode == 1:
            return b""
        raise WorkspaceError("read-only Git command failed")
    return result.stdout


def repository_root(repo) -> Path:
    repo = absolute_path(repo)
    with open_directory(repo):
        pass
    try:
        root = Path(git(repo, "rev-parse", "--show-toplevel").decode("utf-8").rstrip("\n"))
    except UnicodeError as exc:
        raise WorkspaceError("repository path is not UTF-8") from exc
    if root != repo:
        raise WorkspaceError("repo must be the explicit Git worktree root")
    if git(repo, "rev-parse", "--is-bare-repository").strip() != b"false":
        raise WorkspaceError("bare repositories are not supported")
    if git(repo, "config", "--name-only", "--get-regexp",
           r"^filter\..*\.(clean|process)$", optional=True).strip():
        raise WorkspaceError("configured Git clean/process filters prevent a read-only snapshot")
    return repo


def decode_paths(encoded: bytes) -> list[str]:
    if encoded and not encoded.endswith(b"\0"):
        raise WorkspaceError("invalid Git path output")
    try:
        paths = [relative_path(value.decode("utf-8")) for value in encoded.split(b"\0") if value]
    except UnicodeError as exc:
        raise WorkspaceError("snapshot paths must be UTF-8") from exc
    if len(paths) != len(set(paths)):
        raise WorkspaceError("duplicate Git paths")
    return paths


def capture(repo: Path) -> dict:
    head = git(repo, "rev-parse", "--verify", "HEAD").strip()
    if not re.fullmatch(rb"[0-9a-f]{40}|[0-9a-f]{64}", head):
        raise WorkspaceError("repository must have a valid committed HEAD")
    branch = git(repo, "symbolic-ref", "--quiet", "--short", "HEAD", optional=True).rstrip(b"\n")
    diff_flags = ["--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-renames"]
    return {
        "git/head.txt": head + b"\n",
        "git/branch.txt": branch + b"\n",
        "git/status.porcelain": git(repo, "status", "--porcelain=v1", "-z", "--untracked-files=all"),
        "git/staged.patch": git(repo, "diff", "--cached", *diff_flags, "HEAD", "--"),
        "git/unstaged.patch": git(repo, "diff", *diff_flags, "--"),
        "git/tracked.patch": git(repo, "diff", *diff_flags, "HEAD", "--"),
        "git/untracked.paths": git(repo, "ls-files", "--others", "--exclude-standard", "-z"),
        "git/changed.paths": git(repo, "diff", "--name-only", "--no-renames", "-z", "HEAD", "--"),
        # Includes tracked paths with index-only changes undone in the worktree.
        "git/index-changed.paths": git(repo, "diff", "--cached", "--name-only", "--no-renames", "-z", "HEAD", "--"),
    }
