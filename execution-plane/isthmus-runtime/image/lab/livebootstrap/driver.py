"""Bounded two-line IPC with a trusted Docker-administration test coordinator."""
import json
import os
import resource
import selectors
import signal
import subprocess
import time

from lab.identity import _object
from lab.livebootstrap.policy import public_config
from lab.toolchain import BASE_ID

TRUE_FIELDS = {"actual_docker_exec", "authenticated_enrollment", "independent_keys", "same_key_leaf_stable",
               "cross_install_rejected", "wrong_ca_rejected", "lease_retry_rejected", "public_install_retry", "tcp_ready"}
FALSE_FIELDS = {"cross_container_mtls", "production_ready"}
STAGES = {"input", "authority", "control", "output", "containers", "engine", "internal", "arguments",
          "engine-ping", "authority-seed", "request", "pre-ready", "enroll", "key-retry", "leaf-retry",
          "keys", "cross-install", "wrong-ca", "install", "install-retry", "ready", "lease-revoke",
          "lease-denial", "final-policy", "deadline"}


def summary(value):
    if (not isinstance(value, dict) or set(value) != TRUE_FIELDS | FALSE_FIELDS
            or any(value[key] is not True for key in TRUE_FIELDS)
            or any(value[key] is not False for key in FALSE_FIELDS)):
        raise ValueError("live_summary_rejected")
    return value


class Driver:
    def __init__(self, lab):
        self.lab = lab
        self.process = None
        self.selector = selectors.DefaultSelector()
        self.pending = bytearray()
        self.total = 0

    def __enter__(self):
        # No user workloads enter this administrator. UID is not a security
        # boundary for any process allowed to talk to the rootful daemon.
        def no_core():
            resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
        try:
            self.process = subprocess.Popen(["/usr/bin/timeout", "--kill-after=3", "55",
                str(self.lab.root / "live-bin/bootstrap-driver")],
                stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                env={"PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
                     "GOMAXPROCS": "1", "GOMEMLIMIT": "128MiB"},
                start_new_session=True, preexec_fn=no_core)
            self.selector.register(self.process.stdout, selectors.EVENT_READ, "stdout")
            self.selector.register(self.process.stderr, selectors.EVENT_READ, "stderr")
            self.send({"owner": self.lab.name, "image_id": BASE_ID, "worker_path": str(self.lab.root / "live-bin/worker")})
            self.public = public_config(self.line(8))
            return self
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def send(self, value):
        data = json.dumps(value, separators=(",", ":")).encode("ascii") + b"\n"
        if len(data) > 4096 or self.process.stdin.write(data) != len(data):
            raise ValueError("live_driver_input_rejected")
        self.process.stdin.flush()

    def read(self, deadline):
        remaining = deadline - time.monotonic()
        if remaining <= 0 or not self.selector.get_map():
            raise ValueError("live_driver_deadline_or_eof")
        self.lab.capacity()
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise ValueError("live_driver_deadline_or_eof")
        events = self.selector.select(min(remaining, 0.1))
        if time.monotonic() >= deadline:
            raise ValueError("live_driver_deadline_or_eof")
        for key, _ in events:
            data = os.read(key.fileobj.fileno(), 4097)
            if time.monotonic() >= deadline:
                raise ValueError("live_driver_deadline_or_eof")
            if not data:
                self.selector.unregister(key.fileobj)
                continue
            self.total += len(data)
            if self.total > 8192 or key.data == "stderr":
                # Do not record unexpected output, certificate contents or
                # errors from the Docker daemon in a public evidence file.
                if self.total <= 8192 and key.data == "stderr":
                    for stage in STAGES:
                        if data == ("docker bootstrap probe rejected: " + stage + "\n").encode("ascii"):
                            self.lab.save("live-driver-failure.json", {"phase": stage})
                            break
                raise ValueError("live_driver_output_rejected")
            self.pending.extend(data)
            if len(self.pending) > 4096:
                raise ValueError("live_driver_line_limit")

    def line(self, timeout):
        deadline = time.monotonic() + timeout
        while b"\n" not in self.pending:
            self.read(deadline)
        if time.monotonic() >= deadline:
            raise ValueError("live_driver_deadline_or_eof")
        raw, _, remainder = self.pending.partition(b"\n")
        self.pending[:] = remainder
        value = json.loads(raw, object_pairs_hook=_object)
        if time.monotonic() >= deadline:
            raise ValueError("live_driver_deadline_or_eof")
        return value

    def finish(self, cids):
        deadline = time.monotonic() + 33
        self.send({"container_ids": cids})
        self.process.stdin.close()
        value = summary(self.line(30))
        deadline = min(deadline, time.monotonic() + 3)
        while self.selector.get_map():
            self.read(deadline)
        remaining = deadline - time.monotonic()
        if remaining <= 0 or self.pending or self.process.wait(timeout=remaining) != 0 or time.monotonic() >= deadline:
            raise ValueError("live_driver_completion_rejected")
        return value

    def __exit__(self, *_):
        if self.process is not None:
            if self.process.poll() is None:
                try:
                    os.killpg(self.process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            self.process.wait(timeout=5)
            for stream in (self.process.stdin, self.process.stdout, self.process.stderr):
                stream.close()
        self.selector.close()
