import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import stage
from imagekit.lock import ImageError
from test.test_lock import fixture


class ImageCLITests(unittest.TestCase):
    def invoke(self, arguments):
        output, errors = io.StringIO(), io.StringIO()
        code = stage.main(arguments, stdout=output, stderr=errors)
        return code, output.getvalue(), errors.getvalue()

    def test_real_cli_stages_and_verifies_without_external_calls(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            source = root / "packages"
            source.mkdir(mode=0o700)
            document = fixture()
            for package in document["packages"]:
                (source / package["file"]).write_bytes(b"!<arch>\nsynthetic-not-an-installable-package")
            lock = root / "input.json"
            lock.write_text(json.dumps(document))
            destination = root / "context"
            with patch("socket.socket", side_effect=AssertionError("network forbidden")), \
                 patch("subprocess.Popen", side_effect=AssertionError("execution forbidden")), \
                 patch.dict(os.environ, {"HTTPS_PROXY": "synthetic-private-value", "DOCKER_HOST": "synthetic-private-value"}):
                code, output, errors = self.invoke(["stage", "--lock", str(lock), "--source-root", str(source), "--destination", str(destination)])
                self.assertEqual((code, errors), (0, ""))
                expected = {"ok": True, "status": "base_context_verified", "image_built": False,
                            "execution_permitted": False, "artifact_count": 4,
                            "total_bytes": sum(p["size"] for p in document["packages"])}
                self.assertEqual(json.loads(output), expected)
                code, output, errors = self.invoke(["verify", "--directory", str(destination)])
                self.assertEqual((code, errors), (0, ""))
                self.assertEqual(json.loads(output), expected)
            self.assertNotIn("synthetic-private-value", output + errors)
            self.assertNotIn(str(root), output + errors)

    def test_argument_and_dependency_failures_never_echo_values(self):
        for arguments in ([], ["stage"], ["run", "synthetic-private-value"],
                          ["verify", "--directory", "/synthetic-private-value", "--unknown", "synthetic-private-value"],
                          ["verify", "--dir", "/synthetic-private-value"]):
            with self.subTest(argument_count=len(arguments)):
                code, output, errors = self.invoke(arguments)
                self.assertEqual((code, output), (2, ""))
                self.assertEqual(json.loads(errors), {"ok": False, "error_type": "CLIUsageError"})
                self.assertNotIn("synthetic-private-value", errors)
        with patch("stage.verify_context", side_effect=ImageError("synthetic-private-value")):
            code, output, errors = self.invoke(["verify", "--directory", "/synthetic-private-value"])
        self.assertEqual((code, output), (2, ""))
        self.assertEqual(json.loads(errors), {"ok": False, "error_type": "ImageError"})

    def test_unknown_or_capability_promoting_summary_is_not_emitted(self):
        summary = {"status": "base_context_verified", "image_built": False, "execution_permitted": False,
                   "artifact_count": 4, "total_bytes": 128, "source": "synthetic-private-value"}
        with patch("stage.verify_context", return_value=summary):
            code, output, errors = self.invoke(["verify", "--directory", "/synthetic/path"])
        self.assertEqual((code, errors), (0, ""))
        self.assertNotIn("source", output)
        for field, value in (("status", "synthetic-private-value"), ("image_built", True), ("execution_permitted", True),
                             ("artifact_count", True), ("total_bytes", "synthetic-private-value")):
            with patch("stage.verify_context", return_value={**summary, field: value}):
                code, output, errors = self.invoke(["verify", "--directory", "/synthetic/path"])
            self.assertEqual((code, output), (2, ""))
            self.assertNotIn("synthetic-private-value", errors)
