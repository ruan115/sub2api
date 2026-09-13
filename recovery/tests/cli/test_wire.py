import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch

from recoverykit.cli.main import main
from ..wire.support import WireFixtureCase


class WireCLITests(unittest.TestCase):
    arguments = ["wire", "verify", "--observations", "/private/observations.json",
                 "--manifest", "/private/manifest.json", "--source-root", "/private/source",
                 "--catalog-root", "/project/recovery/contracts"]

    def invoke(self, arguments):
        output, errors = io.StringIO(), io.StringIO()
        status = main(arguments, stdout=output, stderr=errors)
        return status, output.getvalue(), errors.getvalue()

    def test_explicit_paths_and_summary_only(self):
        report = {"status": "source_anchored", "business_verification": False,
                  "counts": {"artifacts": 1, "observations": 2, "records": 1, "anchors": 2},
                  "statement": "never echo this statement", "literal": "never echo source",
                  "source_root": "/private/sensitive-path", "observations": ["private"]}
        with patch("recoverykit.cli.main.validate_observations", return_value=report) as verify:
            status, output, errors = self.invoke(self.arguments)
        self.assertEqual(status, 0)
        self.assertEqual(errors, "")
        self.assertEqual(json.loads(output), {"ok": True, "status": "source_anchored",
                         "business_verification": False, "counts": report["counts"]})
        verify.assert_called_once_with(Path("/private/observations.json"), Path("/private/manifest.json"),
                                       Path("/private/source"), Path("/project/recovery/contracts"))

    def test_errors_never_echo_observations_or_paths(self):
        with patch("recoverykit.cli.main.validate_observations", side_effect=ValueError("private-source-and-secret")):
            status, output, errors = self.invoke(self.arguments)
        self.assertEqual(status, 2)
        self.assertEqual(output, "")
        self.assertEqual(json.loads(errors), {"ok": False, "error_type": "ValueError"})

    def test_missing_unknown_and_abbreviated_arguments_fail_closed(self):
        for arguments in (["wire"], ["wire", "verify"], self.arguments[:-2],
                          self.arguments + ["--upload", "private"],
                          ["--observ" if value == "--observations" else value for value in self.arguments]):
            with self.subTest(arguments=arguments), patch("recoverykit.cli.main.validate_observations") as verify:
                status, output, errors = self.invoke(arguments)
            self.assertEqual(status, 2)
            self.assertEqual(output, "")
            self.assertEqual(json.loads(errors)["error_type"], "CLIUsageError")
            verify.assert_not_called()


class WireCLIIntegrationTests(WireFixtureCase):
    def test_catalog_ancestor_link_fails_without_echoing_target(self):
        alias = self.root / "catalog-parent-link"
        alias.symlink_to(self.catalog_root.parent, target_is_directory=True)
        arguments = ["wire", "verify", "--observations", str(self.document_path),
                     "--manifest", str(self.manifest_path), "--source-root", str(self.source_root),
                     "--catalog-root", str(alias / self.catalog_root.name)]
        output, errors = io.StringIO(), io.StringIO()
        self.assertEqual(main(arguments, stdout=output, stderr=errors), 2)
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(json.loads(errors.getvalue()), {"ok": False, "error_type": "WireError"})

    def test_real_validator_success_and_tampering_emit_only_safe_summaries(self):
        arguments = ["wire", "verify", "--observations", str(self.document_path),
                     "--manifest", str(self.manifest_path), "--source-root", str(self.source_root),
                     "--catalog-root", str(self.catalog_root)]
        output, errors = io.StringIO(), io.StringIO()
        self.assertEqual(main(arguments, stdout=output, stderr=errors), 0)
        self.assertEqual(errors.getvalue(), "")
        self.assertEqual(json.loads(output.getvalue()), {
            "ok": True, "status": "source_anchored", "business_verification": False,
            "counts": {"artifacts": 1, "observations": 1, "records": 1, "anchors": 1},
        })

        (self.source_root / self.asset_path).write_bytes(self.source + b"// tampered\n")
        output, errors = io.StringIO(), io.StringIO()
        self.assertEqual(main(arguments, stdout=output, stderr=errors), 2)
        self.assertEqual(output.getvalue(), "")
        self.assertEqual(json.loads(errors.getvalue()), {"ok": False, "error_type": "WireError"})
