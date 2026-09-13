"""Bounded POSIX child output with a single deadline and owned-group cleanup."""

from __future__ import annotations

from dataclasses import dataclass
import math
import os
import selectors
import signal
import subprocess
import time
from typing import Mapping, Sequence


READ_CHUNK_BYTES = 64 * 1024
DEFAULT_STDERR_LIMIT_BYTES = 1024 * 1024
CLEANUP_TIMEOUT_SECONDS = 1.0


class ProcessError(ValueError):
    """Fixed categories only; child output and command arguments are not echoed."""


@dataclass(frozen=True)
class ProcessResult:
    returncode: int
    stdout: bytes


def _remaining(deadline: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise ProcessError("timeout")
    return remaining


def _kill_and_reap(process: subprocess.Popen) -> None:
    # start_new_session below makes this PID the ID of a group created solely
    # for this invocation. Never signal the caller's process group. Kill the
    # group even if the leader exited: its children may still hold pipe ends.
    # Once wait() has reaped a completed leader its PID can be reused. Never
    # signal that numeric group after reaping; a finished invocation already
    # needs no termination. During pipe/deadline failures we have not polled
    # or reaped the child yet, so the owned group ID cannot be reused.
    if process.returncode is not None:
        return
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    try:
        process.wait(timeout=CLEANUP_TIMEOUT_SECONDS)
    except subprocess.TimeoutExpired as error:
        raise ProcessError("cleanup_timeout") from error


def run_bounded(
    command: Sequence[str],
    *,
    environment: Mapping[str, str],
    stdout_limit: int,
    stderr_limit: int = DEFAULT_STDERR_LIMIT_BYTES,
    timeout_seconds: float = 60.0,
) -> ProcessResult:
    """Collect capped stdout and discard capped stderr without communicate().

    Each read is at most the remaining stream budget plus one byte, so size
    rejection happens before unbounded retention. A descendant keeping a pipe
    open cannot bypass the deadline. Failure kills only the newly owned group,
    closes both readers and reaps the direct child without draining its pipes.
    """

    if (os.name != "posix" or type(stdout_limit) is not int or stdout_limit < 0
            or type(stderr_limit) is not int or stderr_limit < 0
            or isinstance(timeout_seconds, bool)
            or not isinstance(timeout_seconds, (int, float))
            or not math.isfinite(timeout_seconds) or timeout_seconds <= 0):
        raise ProcessError("invalid_options")
    deadline = time.monotonic() + timeout_seconds
    try:
        process = subprocess.Popen(
            command, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            stdin=subprocess.DEVNULL, env=environment, bufsize=0,
            start_new_session=True,
        )
    except (OSError, ValueError) as error:
        raise ProcessError("unavailable") from error

    output = bytearray()
    counts = {"stdout": 0, "stderr": 0}
    limits = {"stdout": stdout_limit, "stderr": stderr_limit}
    succeeded = False
    try:
        with selectors.DefaultSelector() as selector:
            for name, pipe in (("stdout", process.stdout), ("stderr", process.stderr)):
                os.set_blocking(pipe.fileno(), False)
                selector.register(pipe, selectors.EVENT_READ, name)
            while selector.get_map():
                events = selector.select(_remaining(deadline))
                for key, _ in events:
                    _remaining(deadline)
                    name = key.data
                    try:
                        data = os.read(key.fd, min(READ_CHUNK_BYTES, limits[name] - counts[name] + 1))
                    except BlockingIOError:
                        continue
                    if not data:
                        selector.unregister(key.fileobj)
                        continue
                    counts[name] += len(data)
                    if counts[name] > limits[name]:
                        raise ProcessError("output_limit")
                    if name == "stdout":
                        output.extend(data)
            returncode = process.wait(timeout=_remaining(deadline))
        # Both pipes have reached EOF and the direct child has been reaped.
        # Preserve the caller's return-code policy (Git's optional code 1).
        succeeded = True
        return ProcessResult(returncode=returncode, stdout=bytes(output))
    except subprocess.TimeoutExpired as error:
        raise ProcessError("timeout") from error
    except OSError as error:
        raise ProcessError("io_failure") from error
    finally:
        try:
            if not succeeded:
                _kill_and_reap(process)
        finally:
            process.stdout.close()
            process.stderr.close()
