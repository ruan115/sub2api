import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch

from recoverykit.cli.main import main


class CLITests(unittest.TestCase):
    def run_cli(self, args):
        output, errors = io.StringIO(), io.StringIO()
        status = main(args, stdout=output, stderr=errors)
        return status, output.getvalue(), errors.getvalue()

    def test_catalog_does_not_claim_business_verification(self):
        with patch("recoverykit.cli.main.validate_catalog", return_value={
            "valid": True, "manifest_count": 2, "entry_count": 3,
            "business_verification": False, "entries": ["do-not-echo"],
        }) as validate:
            status, output, errors = self.run_cli(["contracts", "--root", "catalog"])
        self.assertEqual(status, 0)
        self.assertEqual(errors, "")
        self.assertFalse(json.loads(output)["business_verification"])
        self.assertNotIn("do-not-echo", output)
        validate.assert_called_once_with(Path("catalog"))

    def test_invalid_arguments_do_not_echo_values(self):
        for args in ([], ["secret-fixture-value"],
                     ["contracts", "--root", "catalog", "--secret-fixture-value"]):
            with self.subTest(args_count=len(args)):
                status, output, errors = self.run_cli(args)
            self.assertEqual(status, 2)
            self.assertEqual(output, "")
            self.assertEqual(json.loads(errors), {"ok": False, "error_type": "CLIUsageError"})
            self.assertNotIn("secret-fixture-value", errors)

    def test_errors_do_not_echo_input_content(self):
        with patch("recoverykit.cli.main.validate_catalog", side_effect=ValueError("secret-fixture-value")):
            status, output, errors = self.run_cli(["contracts", "--root", "catalog"])
        self.assertEqual(status, 2)
        self.assertEqual(output, "")
        self.assertEqual(json.loads(errors), {"ok": False, "error_type": "ValueError"})
        self.assertNotIn("secret-fixture-value", errors)

    def test_evidence_verify_never_copies(self):
        with patch("recoverykit.cli.main.load_manifest", return_value={"id": "fixture"}), \
             patch("recoverykit.cli.main.verify_manifest", return_value={"status": "verified", "file_count": 1}) as verify, \
             patch("recoverykit.cli.main.preserve_manifest") as preserve:
            status, output, _ = self.run_cli(["evidence", "verify", "--manifest", "manifest.json", "--source-root", "source"])
        self.assertEqual(status, 0)
        self.assertEqual(json.loads(output)["file_count"], 1)
        verify.assert_called_once_with({"id": "fixture"}, Path("source"))
        preserve.assert_not_called()

    def test_allowlisted_metadata_is_screened_before_output(self):
        synthetic = "ghp_" + "z" * 40
        for result in ({"manifest_id": synthetic}, {"branch": synthetic},
                       {"owners": {"portunex": {"note": synthetic}}}):
            with self.subTest(field=next(iter(result))), patch(
                "recoverykit.cli.main.validate_catalog", return_value=result
            ):
                status, output, errors = self.run_cli(["contracts", "--root", "catalog"])
            self.assertEqual(status, 2)
            self.assertEqual(output, "")
            self.assertEqual(json.loads(errors)["error_type"], "EvidenceError")
            self.assertNotIn(synthetic, errors)

    def test_preserve_uses_explicit_paths(self):
        with patch("recoverykit.cli.main.load_manifest", return_value={"id": "fixture"}), \
             patch("recoverykit.cli.main.preserve_manifest", return_value={"status": "preserved"}) as preserve:
            status, _, _ = self.run_cli(["evidence", "preserve", "--manifest", "manifest.json", "--source-root", "source", "--destination", "/private/new"])
        self.assertEqual(status, 0)
        preserve.assert_called_once_with({"id": "fixture"}, Path("source"), Path("/private/new"))

    def test_workspace_snapshot_is_explicit(self):
        with patch("recoverykit.cli.main.snapshot_workspace", return_value={"status": "created"}) as snapshot:
            status, _, _ = self.run_cli(["workspace", "snapshot", "--repo", "/project", "--destination", "/private/new"])
        self.assertEqual(status, 0)
        snapshot.assert_called_once_with(Path("/project"), Path("/private/new"))

    def test_verify_preserved_and_workspace(self):
        for command, function in (("evidence", "verify_preserved"), ("workspace", "verify_snapshot")):
            operation = "verify-preserved" if command == "evidence" else "verify"
            with self.subTest(command=command), patch("recoverykit.cli.main." + function, return_value={"status": "verified"}) as verify:
                status, _, _ = self.run_cli([command, operation, "--directory", "/private/existing"])
                self.assertEqual(status, 0)
                verify.assert_called_once_with(Path("/private/existing"))


if __name__ == "__main__":
    unittest.main()
