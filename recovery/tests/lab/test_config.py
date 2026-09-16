import os
from pathlib import Path
import socket
import tempfile
import unittest

from recoverykit.lab.config import LabConfig, LabError


class LabConfigTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name).resolve()
        self.path = self.root / "engine.sock"
        self.server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.server.bind(str(self.path))
        self.addCleanup(self.server.close)
        self.config = LabConfig(self.path, "synthetic-engine-id", "ccmax-lab-test", "arm64")

    def test_private_socket_is_explicit_and_stable(self):
        self.config.validate_inputs({})
        self.assertEqual(self.config.socket_identity(), self.config.socket_identity())

    def test_ambient_routing_and_credentials_are_rejected_without_values(self):
        for name in ("DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY",
                     "DOCKER_CERT_PATH", "DOCKER_API_VERSION", "DOCKER_AUTH_CONFIG",
                     "HTTP_PROXY", "https_proxy", "ALL_PROXY", "NO_PROXY", "SSH_AUTH_SOCK"):
            with self.subTest(name=name), self.assertRaises(LabError) as caught:
                self.config.validate_inputs({name: "synthetic-private-value"})
            self.assertNotIn("synthetic-private-value", str(caught.exception))
        self.config.validate_inputs({"HTTPS_PROXY": "", "PATH": "ignored"})

    def test_remote_relative_global_and_malformed_paths_are_rejected(self):
        for raw in ("tcp://127.0.0.1:2375", "ssh://server", "relative.sock", "/var/run/docker.sock",
                    "/run/docker.sock", "/tmp/../engine.sock", "/tmp/a\n.sock", "/tmp/a\\b.sock"):
            with self.subTest(raw=raw), self.assertRaises(LabError):
                LabConfig(Path(raw), "engine", "ccmax-lab", "arm64").validate_inputs({})
        for field in ("engine_id", "engine_name", "architecture"):
            with self.subTest(field=field), self.assertRaises(LabError):
                values = dict(socket_path=self.path, engine_id="engine", engine_name="ccmax-lab", architecture="arm64")
                values[field] = "bad\nvalue"
                LabConfig(**values).validate_inputs({})

    def test_socket_symlink_regular_file_and_public_parent_are_rejected(self):
        link = self.root / "link.sock"
        link.symlink_to(self.path)
        file = self.root / "file.sock"
        file.write_bytes(b"synthetic fixture")
        for path in (link, file, self.root / "missing.sock"):
            with self.subTest(kind=path.name), self.assertRaises(LabError):
                LabConfig(path, "engine", "ccmax-lab", "arm64").socket_identity()
        self.root.chmod(0o755)
        with self.assertRaises(LabError):
            self.config.socket_identity()
        self.root.chmod(0o700)

    def test_symlink_ancestor_is_rejected(self):
        nested = self.root / "alias"
        nested.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(LabError):
            LabConfig(nested / "engine.sock", "engine", "ccmax-lab", "arm64").socket_identity()

    def test_socket_replacement_changes_identity(self):
        first = self.config.socket_identity()
        self.path.unlink()
        replacement = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.addCleanup(replacement.close)
        replacement.bind(str(self.path))
        self.assertNotEqual(first, self.config.socket_identity())
