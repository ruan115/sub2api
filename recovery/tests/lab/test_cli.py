import io
import json
import os
import unittest
from unittest.mock import patch

from recoverykit.cli.main import main
from recoverykit.lab.config import LabError
from tests.lab.test_engine import LocalUnixFixture, response
from tests.lab.test_preflight import PATHS, metadata_fixture


class LabCLITests(unittest.TestCase):
    def invoke(self, arguments):
        output, errors = io.StringIO(), io.StringIO()
        code = main(arguments, stdout=output, stderr=errors)
        return code, output.getvalue(), errors.getvalue()

    def arguments(self, socket):
        return ["lab", "inspect", "--socket", str(socket),
                "--expected-engine-id", "synthetic-engine-id",
                "--expected-engine-name", "ccmax-lab-test", "--expected-architecture", "arm64"]

    def test_actual_cli_to_unix_http_checks_metadata_without_authorizing_execution(self):
        documents = metadata_fixture()
        documents[PATHS[1]]["DockerRootDir"] = "/synthetic-private-value"
        documents[PATHS[1]]["ExtraField"] = "synthetic-private-value"

        def serve(fixture, sock):
            path = fixture.requests[-1][0].split()[1]
            sock.sendall(response(json.dumps(documents[path]).encode()))

        fixture = LocalUnixFixture(self, serve)
        with patch.dict(os.environ, {}, clear=True), patch("socket.create_connection", side_effect=AssertionError("TCP forbidden")):
            code, output, errors = self.invoke(self.arguments(fixture.path))
        self.assertEqual(code, 0)
        self.assertEqual(errors, "")
        self.assertEqual(json.loads(output), {
            "ok": True, "status": "docker_endpoint_checked", "isolation_verified": False,
            "execution_permitted": False, "created_resources": 0,
            "container_count": 0, "volume_count": 0, "network_count": 3,
        })
        self.assertEqual([request[0].split()[1] for request in fixture.requests], list(PATHS))
        self.assertTrue(all(request[0].startswith("GET ") for request in fixture.requests))
        self.assertNotIn("synthetic-private-value", output + errors)

    def test_identity_failure_stops_before_listing_resources(self):
        documents = metadata_fixture()
        documents[PATHS[1]]["ID"] = "another-daemon"

        def serve(fixture, sock):
            path = fixture.requests[-1][0].split()[1]
            sock.sendall(response(json.dumps(documents[path]).encode()))

        fixture = LocalUnixFixture(self, serve)
        with patch.dict(os.environ, {}, clear=True):
            code, output, errors = self.invoke(self.arguments(fixture.path))
        self.assertEqual(code, 2)
        self.assertEqual(output, "")
        self.assertEqual(json.loads(errors), {"ok": False, "error_type": "LabError"})
        self.assertEqual([request[0].split()[1] for request in fixture.requests], list(PATHS[:2]))

    def test_bad_arguments_and_dependency_errors_are_redacted(self):
        for args in (["lab"], ["lab", "inspect"], ["lab", "run", "synthetic-private-value"],
                     self.arguments("/synthetic/socket") + ["--unexpected", "synthetic-private-value"]):
            with self.subTest(args_count=len(args)):
                code, output, errors = self.invoke(args)
                self.assertEqual(code, 2)
                self.assertEqual(output, "")
                self.assertNotIn("synthetic-private-value", errors)
        with patch("recoverykit.cli.main.inspect_lab", side_effect=LabError("synthetic-private-value")):
            code, output, errors = self.invoke(self.arguments("/synthetic/socket"))
        self.assertEqual((code, output), (2, ""))
        self.assertNotIn("synthetic-private-value", errors)

    def test_ambient_proxy_rejected_before_any_request(self):
        fixture = LocalUnixFixture(self, lambda _, sock: sock.sendall(response()))
        with patch.dict(os.environ, {"HTTPS_PROXY": "synthetic-private-value"}, clear=True):
            code, output, errors = self.invoke(self.arguments(fixture.path))
        self.assertEqual((code, output), (2, ""))
        self.assertEqual(fixture.requests, [])
        self.assertNotIn("synthetic-private-value", errors)
