import http.client
import json
from pathlib import Path
import socket
import socketserver
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

from recoverykit.lab.config import LabConfig, LabError
from recoverykit.lab.engine import UnixEngine


def response(body=b"{}", status=b"200 OK", extra=b"", length=True):
    return (b"HTTP/1.1 " + status + b"\r\nContent-Type: application/json\r\nConnection: close\r\n"
            + (b"Content-Length: " + str(len(body)).encode() + b"\r\n" if length else b"")
            + extra + b"\r\n" + body)


class LocalUnixFixture:
    def __init__(self, test, action):
        self.directory = tempfile.TemporaryDirectory()
        test.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name).resolve() / "engine.sock"
        self.requests = []
        self.release = threading.Event()
        fixture = self

        class Handler(socketserver.StreamRequestHandler):
            def handle(self):
                self.request.settimeout(1)
                request_line = self.rfile.readline(1024).decode("ascii").strip()
                headers = []
                while True:
                    header = self.rfile.readline(1024)
                    if header in (b"\r\n", b"\n", b""):
                        break
                    headers.append(header)
                fixture.requests.append((request_line, headers))
                try:
                    action(fixture, self.request)
                except (BrokenPipeError, ConnectionResetError, socket.timeout):
                    pass

        self.server = socketserver.ThreadingUnixStreamServer(str(self.path), Handler)
        self.server.daemon_threads = True
        self.thread = threading.Thread(target=self.server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
        self.thread.start()
        test.addCleanup(self.close)
        self.config = LabConfig(self.path, "synthetic-engine-id", "ccmax-lab-test", "arm64")

    def close(self):
        self.release.set()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=1)

    def engine(self):
        return UnixEngine(self.config, environment={})


class UnixEngineTests(unittest.TestCase):
    def test_actual_unix_get_has_no_auth_proxy_or_tcp_fallback(self):
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(response(b'{"synthetic":true}')))
        with patch("socket.create_connection", side_effect=AssertionError("TCP forbidden")):
            result = fixture.engine().get_json("/version")
        self.assertEqual(result, {"synthetic": True})
        self.assertEqual(len(fixture.requests), 1)
        self.assertEqual(fixture.requests[0][0], "GET /version HTTP/1.1")
        headers = b"".join(fixture.requests[0][1]).lower()
        self.assertNotIn(b"authorization", headers)
        self.assertNotIn(b"cookie", headers)
        self.assertNotIn(b"proxy-", headers)

    def test_unknown_paths_and_write_targets_never_connect(self):
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(response()))
        client = fixture.engine()
        for path in ("/build", "/containers/create", "/v1.43/containers/id", "http://remote.invalid", "/version?secret=x"):
            with self.subTest(path=path), self.assertRaises(LabError):
                client.get_json(path)
        self.assertEqual(fixture.requests, [])

    def test_socket_replaced_before_or_during_response_is_rejected(self):
        def replace(fixture, sock):
            fixture.path.unlink()
            replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            replacement.bind(str(fixture.path))
            self.addCleanup(replacement.close)
            sock.sendall(response())

        fixture = LocalUnixFixture(self, replace)
        with self.assertRaisesRegex(LabError, "socket_changed"):
            fixture.engine().get_json("/version")
        other = LocalUnixFixture(self, lambda _, sock: sock.sendall(response()))
        client = other.engine()
        other.path.unlink()
        with self.assertRaises(LabError):
            client.get_json("/version")
        self.assertEqual(other.requests, [])

    def test_bad_json_status_framing_and_oversize_fail_closed(self):
        cases = {
            "duplicate": response(b'{"ID":"a","ID":"b"}'),
            "nan": response(b'{"value":NaN}'),
            "infinity": response(b'{"value":1e9999}'),
            "huge integer": response(b'{"value":' + b"9" * 100 + b"}"),
            "wrong shape": response(b"true"),
            "invalid utf8": response(b'"\xff"'),
            "truncated": response(b"{}").replace(b"Content-Length: 2", b"Content-Length: 20"),
            "declared oversize": response().replace(b"Content-Length: 2", b"Content-Length: 999999999"),
            "huge length digits": response().replace(b"Content-Length: 2", b"Content-Length: " + b"9" * 300),
            "actual oversize": response(b"x" * 257, length=False),
            "gzip": response(extra=b"Content-Encoding: gzip\r\n"),
            "duplicate length": response(extra=b"Content-Length: 2\r\n"),
            "length plus chunked": response(extra=b"Transfer-Encoding: chunked\r\n"),
            "wrong type": response().replace(b"application/json", b"text/plain"),
            "redirect": response(status=b"302 Found", extra=b"Location: http://remote.invalid/\r\n"),
            "error with secret": response(b"synthetic-private-value", status=b"500 Error"),
        }
        for name, payload in cases.items():
            with self.subTest(name=name):
                fixture = LocalUnixFixture(self, lambda _, sock, data=payload: sock.sendall(data))
                with patch("recoverykit.lab.engine.MAX_RESPONSE_BYTES", 256), self.assertRaises(LabError) as caught:
                    fixture.engine().get_json("/version")
                self.assertNotIn("synthetic-private-value", str(caught.exception))
                self.assertEqual(len(fixture.requests), 1)

    def test_chunked_json_is_bounded_and_supported(self):
        payload = response(b"2\r\n{}\r\n0\r\n\r\n", extra=b"Transfer-Encoding: chunked\r\n", length=False)
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(payload))
        self.assertEqual(fixture.engine().get_json("/version"), {})

    def test_header_and_body_slow_drip_cannot_extend_deadline(self):
        for headers in (False, True):
            def drip(fixture, sock):
                if headers:
                    sock.sendall(b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n")
                else:
                    sock.sendall(b"HTTP/1.1 200 OK\r\nX-Slow: ")
                while not fixture.release.wait(0.005):
                    sock.sendall(b" ")

            with self.subTest(headers=headers):
                fixture = LocalUnixFixture(self, drip)
                client = fixture.engine()
                started = time.monotonic()
                with patch("recoverykit.lab.engine.REQUEST_SECONDS", 0.08), self.assertRaises(LabError):
                    client.get_json("/version")
                fixture.release.set()
                self.assertLess(time.monotonic() - started, 0.8)
                with self.assertRaises(LabError):
                    client.get_json("/version")

    def test_total_deadline_and_request_budget_are_not_reset(self):
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(response()))
        client = fixture.engine()
        client.deadline = time.monotonic() - 1
        with self.assertRaises(LabError):
            client.get_json("/version")
        self.assertEqual(fixture.requests, [])
        client = fixture.engine()
        for _ in range(5):
            self.assertEqual(client.get_json("/version"), {})
        with self.assertRaises(LabError):
            client.get_json("/version")
        self.assertEqual(len(fixture.requests), 5)

    def test_connect_crossing_request_deadline_sends_no_get(self):
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(response()))
        original_connect = socket.socket.connect

        def delayed_connect(connection, address):
            time.sleep(0.08)
            return original_connect(connection, address)

        with patch("recoverykit.lab.engine.REQUEST_SECONDS", 0.02), patch.object(socket.socket, "connect", delayed_connect):
            with self.assertRaises(LabError):
                fixture.engine().get_json("/version")
        self.assertFalse(any(request[0].startswith("GET ") for request in fixture.requests))

    def test_json_parse_crossing_request_deadline_is_rejected(self):
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(response()))
        original_loads = json.loads

        def delayed_loads(*args, **kwargs):
            value = original_loads(*args, **kwargs)
            time.sleep(0.08)
            return value

        with patch("recoverykit.lab.engine.REQUEST_SECONDS", 0.02), patch("recoverykit.lab.engine.json.loads", delayed_loads):
            with self.assertRaises(LabError):
                fixture.engine().get_json("/version")

    def test_implicit_http_connection_reconnect_is_forbidden(self):
        from recoverykit.lab.engine import _UnixHTTPConnection
        with patch("socket.create_connection", side_effect=AssertionError("TCP forbidden")), self.assertRaises(LabError):
            _UnixHTTPConnection("synthetic.invalid").connect()
