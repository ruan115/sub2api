"""Unit tests for the offline recovery contract catalog validator."""

from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from recoverykit.contracts import ContractError, format_report, load_catalog, validate_catalog


def _manifest(module_id, owner, entry_id, path):
    return {
        "schema_version": 1,
        "id": module_id,
        "owner": owner,
        "status": "discovered",
        "evidence": [
            {
                "id": "static_asset",
                "kind": "static_asset",
                "source": "asset.js:1",
                "observed": "A literal path was present in a static asset.",
            }
        ],
        "discovered": {
            "api_paths": [
                {
                    "id": entry_id,
                    "path": path,
                    "method": "unknown",
                    "status": "discovered",
                    "evidence": ["static_asset"],
                    "unknowns": ["The HTTP method and response shape are not yet verified."],
                }
            ]
        },
        "unknowns": ["This manifest is discovery evidence, not a complete API contract."],
    }


class ContractCatalogTests(unittest.TestCase):
    def setUp(self):
        # The loader intentionally rejects a symlink anywhere in the input
        # path.  macOS normally exposes its temp directory below the `/var`
        # compatibility symlink, so fixtures use the physical spelling rather
        # than weakening production no-follow handling.
        self._temporary_directory = tempfile.TemporaryDirectory(
            dir=str(Path(tempfile.gettempdir()).resolve())
        )
        self.root = Path(self._temporary_directory.name) / "contracts"
        self.root.mkdir()

    def tearDown(self):
        self._temporary_directory.cleanup()

    def _write(self, owner, module, document):
        path = self.root / owner / module / "manifest.json"
        path.parent.mkdir(parents=True)
        path.write_text(json.dumps(document), encoding="utf-8")
        return path

    def test_valid_catalog_returns_structure_only_report(self):
        self._write(
            "portunex",
            "identity",
            _manifest("portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"),
        )

        catalog = load_catalog(self.root)
        report = validate_catalog(self.root)

        self.assertEqual(1, len(catalog["manifests"]))
        self.assertTrue(report["valid"])
        self.assertFalse(report["business_verification"])
        self.assertEqual(1, report["manifest_count"])
        self.assertEqual(1, report["entry_count"])
        self.assertIn("business verification: false", format_report(report))

    def test_duplicate_stable_entry_id_is_rejected(self):
        duplicate_id = "portunex.shared.api.path"
        self._write(
            "portunex",
            "identity",
            _manifest("portunex.identity", "portunex", duplicate_id, "/portunex/auth/login"),
        )
        self._write(
            "portunex",
            "users",
            _manifest("portunex.users", "portunex", duplicate_id, "/portunex/users/me"),
        )

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("duplicate stable ID", str(raised.exception))

    def test_unknown_evidence_reference_is_rejected(self):
        document = _manifest(
            "portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"
        )
        document["discovered"]["api_paths"][0]["evidence"] = ["missing_asset"]
        self._write("portunex", "identity", document)

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("unknown evidence ID", str(raised.exception))

    def test_discovered_record_without_unknowns_is_rejected(self):
        document = _manifest(
            "portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"
        )
        document["discovered"]["api_paths"][0]["unknowns"] = []
        self._write("portunex", "identity", document)

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("discovered records must declare at least one unknown", str(raised.exception))

    def test_bad_json_field_type_is_rejected(self):
        document = _manifest(
            "portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"
        )
        document["evidence"] = {"not": "an array"}
        self._write("portunex", "identity", document)

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("evidence must be a non-empty JSON array", str(raised.exception))

    def test_duplicate_json_keys_are_rejected_before_last_key_wins(self):
        path = self.root / "portunex" / "identity" / "manifest.json"
        path.parent.mkdir(parents=True)
        path.write_text('{"schema_version": 1, "schema_version": 1}', encoding="utf-8")

        with self.assertRaises(ContractError) as raised:
            load_catalog(self.root)

        self.assertIn("duplicate JSON key", str(raised.exception))

    def test_empty_catalog_is_rejected(self):
        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("must contain at least one manifest", str(raised.exception))

    def test_empty_module_discovery_is_rejected(self):
        document = _manifest(
            "portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"
        )
        document["discovered"] = {}
        self._write("portunex", "identity", document)

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("discovered must contain at least one record", str(raised.exception))

    def test_verified_status_requires_declared_verification_evidence(self):
        document = _manifest(
            "portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"
        )
        document["status"] = "verified"
        document["verification_evidence"] = ["not_declared"]
        self._write("portunex", "identity", document)

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("must reference a declared evidence ID", str(raised.exception))

    def test_same_path_and_method_conflict_is_rejected(self):
        self._write(
            "portunex",
            "identity",
            _manifest("portunex.identity", "portunex", "portunex.identity.api.login", "/portunex/auth/login"),
        )
        self._write(
            "portunex",
            "users",
            _manifest("portunex.users", "portunex", "portunex.users.api.login_alias", "/portunex/auth/login"),
        )

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("API path conflict", str(raised.exception))

    def test_directory_owner_must_match_manifest_owner(self):
        self._write(
            "portunex",
            "identity",
            _manifest("portunex.identity", "isthmus", "portunex.identity.api.login", "/portunex/auth/login"),
        )

        with self.assertRaises(ContractError) as raised:
            validate_catalog(self.root)

        self.assertIn("does not match directory owner", str(raised.exception))

    def test_repository_catalog_is_valid_discovery_only(self):
        repository_root = Path(__file__).resolve().parents[3]
        report = validate_catalog(repository_root / "recovery" / "contracts")

        self.assertTrue(report["valid"])
        self.assertFalse(report["business_verification"])
        self.assertGreaterEqual(report["manifest_count"], 1)
        self.assertGreater(report["unknown_count"], 0)

    def test_database_baseline_lists_the_observed_unique_table_count(self):
        repository_root = Path(__file__).resolve().parents[3]
        baseline_path = repository_root / "recovery" / "baselines" / "portunex" / "database_inventory.json"
        with baseline_path.open(encoding="utf-8") as handle:
            baseline = json.load(handle)

        groups = baseline["discovered"]["business_groups"]
        table_names = [table for group in groups for table in group["tables"]]
        self.assertEqual(36, baseline["discovered"]["observed_table_count"])
        self.assertEqual(baseline["discovered"]["observed_table_count"], len(table_names))
        self.assertEqual(len(table_names), len(set(table_names)))
        self.assertEqual("_sqlx_migrations", baseline["discovered"]["migration_metadata_table"])


if __name__ == "__main__":
    unittest.main()
