"""Offline metadata regressions for the repository's reviewed wire observations.

Production JavaScript remains outside Git. These tests do not read source bytes,
verify source/anchor hashes or literal contents, execute JavaScript, or establish
server behavior. Full source anchoring still requires the separate private-input
``wire verify`` command; passing metadata checks is not ``source_anchored``.
"""

from __future__ import annotations

from pathlib import Path
import unittest

from recoverykit.evidence import load_manifest
from recoverykit.wire.schema import load_observation_document
from recoverykit.wire.validation import _load_api_records_once


class RepositoryWireObservationMetadataTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.recovery_root = Path(__file__).resolve().parents[2]
        cls.observation_paths = {
            module: cls.recovery_root / "contracts-wire" / "portunex" / module / "observations.json"
            for module in ("identity", "users", "apikeys", "providers")
        }

    def load_documents(self):
        # Reuse strict JSON, schema, size and metadata-secret checks rather than
        # introducing a permissive JSON reader for the real repository inputs.
        return {
            module: load_observation_document(path)
            for module, path in self.observation_paths.items()
        }

    def load_artifact_metadata(self):
        # Loading an evidence manifest validates declarations only. It does not
        # open any entry path or imply that its SHA-256 matches a private file.
        return load_manifest(
            self.recovery_root / "baselines" / "portunex" / "static-wire-artifacts.json"
        )

    def test_repository_documents_pass_strict_metadata_schema(self):
        documents = self.load_documents()
        self.assertEqual({"identity", "users", "apikeys", "providers"}, set(documents))
        for module, document in documents.items():
            with self.subTest(module=module):
                self.assertTrue(document["observations"])

    def test_repository_metadata_document_and_observation_ids_are_globally_unique(self):
        document_ids = set()
        observation_ids = set()
        for module, document in self.load_documents().items():
            with self.subTest(module=module):
                self.assertNotIn(document["id"], document_ids)
                document_ids.add(document["id"])
            for observation in document["observations"]:
                with self.subTest(module=module, observation=observation["id"]):
                    self.assertNotIn(observation["id"], observation_ids)
                    observation_ids.add(observation["id"])

    def test_repository_metadata_references_existing_catalog_api_ids_and_exact_paths(self):
        # This is the same fully validated, single-memory API-only index used
        # by wire verification, not an index of page/database discovery IDs.
        api_records = _load_api_records_once(self.recovery_root / "contracts")
        for module, document in self.load_documents().items():
            for observation in document["observations"]:
                with self.subTest(module=module, observation=observation["id"]):
                    self.assertIn(observation["record_id"], api_records)
                    self.assertEqual(api_records[observation["record_id"]], observation["path"])

    def test_repository_metadata_references_the_declared_artifact_manifest(self):
        manifest = self.load_artifact_metadata()
        for module, document in self.load_documents().items():
            with self.subTest(module=module):
                self.assertEqual(manifest["id"], document["artifact_manifest_id"])

    def test_repository_anchor_metadata_fits_allowlisted_declared_file_sizes(self):
        manifest = self.load_artifact_metadata()
        entries = {entry["path"]: entry for entry in manifest["entries"]}
        for module, document in self.load_documents().items():
            for observation in document["observations"]:
                for index, anchor in enumerate(observation["anchors"]):
                    with self.subTest(module=module, observation=observation["id"], anchor=index):
                        self.assertIn(anchor["artifact"], entries)
                        self.assertGreaterEqual(anchor["start"], 0)
                        self.assertLess(anchor["start"], anchor["end"])
                        self.assertLessEqual(anchor["end"], entries[anchor["artifact"]]["size"])
                        # Necessary metadata bounds only: neither this length
                        # check nor the schema's hash syntax proves UTF-8 byte
                        # boundaries, literal membership or actual hash equality.
                        self.assertLessEqual(
                            len(anchor["literal"].encode("utf-8")),
                            anchor["end"] - anchor["start"],
                        )
