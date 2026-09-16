"""Bounded Unix HTTP GETs; no Docker CLI, TCP dial, redirects or write API."""
import http.client
import json
import math
import os
import socket
import threading
import time

from .config import LabError


MAX_RESPONSE_BYTES = 2 * 1024 * 1024
REQUEST_SECONDS = 3.0
TOTAL_SECONDS = 12.0
READ_PATHS = frozenset({
    "/version", "/v1.43/info", "/v1.43/containers/json?all=1",
    "/v1.43/networks", "/v1.43/volumes",
})


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise LabError("duplicate_json_key")
        result[key] = value
    return result


def _reject_constant(_):
    raise LabError("invalid_json_number")


def _integer(raw):
    if len(raw) > 20:
        raise LabError("invalid_json_number")
    return int(raw)


def _float(raw):
    if len(raw) > 64:
        raise LabError("invalid_json_number")
    value = float(raw)
    if not math.isfinite(value):
        raise LabError("invalid_json_number")
    return value


def _shutdown(connection):
    try:
        connection.shutdown(socket.SHUT_RDWR)
    except OSError:
        pass


class _UnixHTTPConnection(http.client.HTTPConnection):
    def connect(self):
        # The caller installs an already connected AF_UNIX socket. If it ever
        # disappears, never allow HTTPConnection's TCP reconnect path.
        raise LabError("implicit_connect_forbidden")


class UnixEngine:
    def __init__(self, config, *, environment=None):
        config.validate_inputs(os.environ if environment is None else environment)
        self.config = config
        self.identity = config.socket_identity()
        self.deadline = time.monotonic() + TOTAL_SECONDS
        self.failed = False
        self.calls = 0

    def verify_endpoint(self):
        if self.config.socket_identity() != self.identity:
            raise LabError("socket_changed")
        if time.monotonic() >= self.deadline:
            raise LabError("engine_timeout")

    def get_json(self, path):
        if path not in READ_PATHS or self.failed or self.calls >= len(READ_PATHS):
            raise LabError("engine_operation_forbidden")
        try:
            self.verify_endpoint()
            self.calls += 1
            value = self._request(path)
            self.verify_endpoint()
            return value
        except LabError:
            self.failed = True
            raise
        except (OSError, ValueError, RecursionError, http.client.HTTPException):
            self.failed = True
            raise LabError("engine_request_failed") from None

    def _request(self, path):
        remaining = min(REQUEST_SECONDS, self.deadline - time.monotonic())
        if remaining <= 0 or not math.isfinite(remaining):
            raise LabError("engine_timeout")
        request_deadline = time.monotonic() + remaining
        raw_socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        client = _UnixHTTPConnection("docker-lab.invalid", timeout=remaining)
        expired = threading.Event()

        def expire():
            expired.set()
            _shutdown(raw_socket)

        def check_deadline():
            if expired.is_set() or time.monotonic() >= request_deadline:
                raise LabError("engine_timeout")

        # socket timeout alone is an inactivity budget: a slow-drip peer can
        # keep resetting it. This timer interrupts header and body reads at a
        # single wall-clock deadline, even before response headers complete.
        timer = threading.Timer(remaining, expire)
        timer.daemon = True
        timer.start()
        response = None
        try:
            raw_socket.settimeout(remaining)
            raw_socket.connect(str(self.config.socket_path))
            self.verify_endpoint()
            check_deadline()
            client.sock = raw_socket
            client.request("GET", path, headers={
                "Accept": "application/json", "Accept-Encoding": "identity", "Connection": "close",
            })
            response = client.getresponse()
            check_deadline()
            if response.status != 200:
                raise LabError("engine_status_rejected")
            if (response.getheader("Content-Type", "").split(";", 1)[0].strip().lower() != "application/json"
                    or response.getheader("Content-Encoding", "identity").lower() != "identity"):
                raise LabError("engine_encoding_rejected")
            lengths = response.headers.get_all("Content-Length", [])
            transfers = response.headers.get_all("Transfer-Encoding", [])
            if len(lengths) > 1 or len(transfers) > 1 or (lengths and transfers):
                raise LabError("engine_framing_rejected")
            length = None
            if lengths:
                if len(lengths[0]) > 10 or not lengths[0].isascii() or not lengths[0].isdigit():
                    raise LabError("engine_framing_rejected")
                length = int(lengths[0])
                if length > MAX_RESPONSE_BYTES:
                    raise LabError("engine_response_limit")
            if transfers and transfers != ["chunked"]:
                raise LabError("engine_framing_rejected")
            chunks = bytearray()
            while True:
                check_deadline()
                data = response.read1(min(64 * 1024, MAX_RESPONSE_BYTES + 1 - len(chunks)))
                if not data:
                    break
                chunks.extend(data)
                if len(chunks) > MAX_RESPONSE_BYTES:
                    raise LabError("engine_response_limit")
            check_deadline()
            if length is not None and len(chunks) != length:
                raise LabError("engine_response_truncated")
            try:
                value = json.loads(chunks.decode("utf-8"), object_pairs_hook=_object,
                                   parse_constant=_reject_constant, parse_int=_integer, parse_float=_float)
            except (ValueError, RecursionError):
                raise LabError("engine_json_rejected") from None
            if not isinstance(value, (dict, list)):
                raise LabError("engine_json_shape_rejected")
            check_deadline()
            return value
        finally:
            timer.cancel()
            _shutdown(raw_socket)
            if response is not None:
                response.close()
            client.close()
            raw_socket.close()
            timer.join(timeout=0.2)
