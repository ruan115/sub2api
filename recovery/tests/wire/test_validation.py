"""Synthetic security and integrity tests for static wire observations."""

from __future__ import annotations

import hashlib
import json
import os
import subprocess
from unittest.mock import patch

from recoverykit.wire import WireError, validate_observations
from recoverykit.wire import validation as wire_validation

from .support import WireFixtureCase


class WireObservationTests(WireFixtureCase):
    def validate(self):
        return validate_observations(
            self.document_path,
            self.manifest_path,
            self.source_root,
            self.catalog_root,
        )

    def assert_wire_error(self, code):
        with self.assertRaises(WireError) as raised:
            self.validate()
        self.assertEqual(code, raised.exception.code)
        self.assertEqual(code, str(raised.exception))

    def test_valid_source_anchoring_returns_safe_non_compatibility_summary(self):
        before = (self.source_root / self.asset_path).stat()
        result = self.validate()
        after = (self.source_root / self.asset_path).stat()

        self.assertEqual(
            {
                "status": "source_anchored",
                "business_verification": False,
                "counts": {"artifacts": 1, "observations": 1, "records": 1, "anchors": 1},
            },
            result,
        )
        rendered = json.dumps(result)
        self.assertNotIn("/portunex/auth/login", rendered)
        self.assertNotIn("method token", rendered)
        self.assertEqual((before.st_mtime_ns, before.st_size), (after.st_mtime_ns, after.st_size))

    def test_source_hash_tampering_fails_without_echoing_source(self):
        target = self.source_root / self.asset_path
        target.write_bytes(self.source + b"// mutation\n")

        self.assert_wire_error("source_integrity")

    def test_utf8_byte_boundaries_and_anchor_hash_literal_are_strict(self):
        document = self.copied_document()
        start = self.source.index("你".encode("utf-8")) + 1
        end = self.source.index(b"\n", start)
        raw_span = self.source[start:end]
        anchor = document["observations"][0]["anchors"][0]
        anchor.update(start=start, end=end, sha256=hashlib.sha256(raw_span).hexdigest(), literal="POST")
        self.write_document(document)
        self.assert_wire_error("invalid_anchor")

        document = self.valid_document()
        document["observations"][0]["anchors"][0]["sha256"] = "0" * 64
        self.write_document(document)
        self.assert_wire_error("invalid_anchor")

        document = self.valid_document()
        document["observations"][0]["anchors"][0]["literal"] = "absent-literal"
        self.write_document(document)
        self.assert_wire_error("invalid_anchor")

    def test_catalog_references_must_be_existing_api_records_with_exact_paths(self):
        document = self.copied_document()
        document["observations"][0]["record_id"] = "portunex.identity.api.missing"
        self.write_document(document)
        self.assert_wire_error("catalog_reference")

        document = self.valid_document()
        document["observations"][0]["path"] = "/portunex/auth/other"
        self.write_document(document)
        self.assert_wire_error("catalog_reference")

    def test_malformed_url_like_path_has_a_stable_wire_error(self):
        document = self.copied_document()
        # This starts with a slash, but urlsplit treats its malformed
        # bracketed authority as an invalid IPv6 URL and raises ValueError.
        document["observations"][0]["path"] = "//["
        self.write_document(document)
        self.assert_wire_error("invalid_document")

    def test_document_rejects_extra_fields_duplicate_ids_booleans_and_duplicate_anchors(self):
        variants = []
        extra = self.copied_document()
        extra["unexpected"] = "field"
        variants.append(extra)
        duplicate_id = self.copied_document()
        duplicate = json.loads(json.dumps(duplicate_id["observations"][0]))
        duplicate["aspect"] = "path"
        duplicate_id["observations"].append(duplicate)
        variants.append(duplicate_id)
        boolean_offset = self.copied_document()
        boolean_offset["observations"][0]["anchors"][0]["start"] = True
        variants.append(boolean_offset)
        duplicate_anchor = self.copied_document()
        duplicate_anchor["observations"][0]["anchors"].append(
            json.loads(json.dumps(duplicate_anchor["observations"][0]["anchors"][0]))
        )
        variants.append(duplicate_anchor)
        for document in variants:
            with self.subTest(variant=len(document)):
                self.write_document(document)
                self.assert_wire_error("invalid_document")

    def test_nested_schema_and_bounded_fields_fail_before_source_interpretation(self):
        nested_extra = self.copied_document()
        nested_extra["observations"][0]["anchors"][0]["unexpected"] = "field"
        self.write_document(nested_extra)
        self.assert_wire_error("invalid_document")

        too_long_statement = self.valid_document()
        too_long_statement["observations"][0]["statement"] = "x" * 8193
        self.write_document(too_long_statement)
        self.assert_wire_error("input_limit")

        too_many_anchors = self.valid_document()
        anchor = too_many_anchors["observations"][0]["anchors"][0]
        too_many_anchors["observations"][0]["anchors"] = [
            dict(anchor, start=index, end=index + 1, sha256=hashlib.sha256(self.source[index:index + 1]).hexdigest(), literal=chr(self.source[index]))
            for index in range(9)
        ]
        self.write_document(too_many_anchors)
        self.assert_wire_error("input_limit")

    def test_document_and_source_secret_screening_are_enforced(self):
        secret = "ghp_" + "a" * 40
        document = self.copied_document()
        document["observations"][0]["statement"] = secret
        self.write_document(document)
        self.assert_wire_error("unsafe_input")

        self.write_document(self.valid_document())
        target = self.source_root / self.asset_path
        changed = self.source + secret.encode("utf-8")
        target.write_bytes(changed)
        self.manifest["entries"][0]["size"] = len(changed)
        self.manifest["entries"][0]["sha256"] = hashlib.sha256(changed).hexdigest()
        self.write_json(self.manifest_path, self.manifest)
        self.assert_wire_error("source_integrity")

    def test_unknown_artifacts_manifest_identity_and_bounds_fail_closed(self):
        document = self.copied_document()
        document["observations"][0]["anchors"][0]["artifact"] = "assets/missing.js"
        self.write_document(document)
        self.assert_wire_error("invalid_anchor")

        document = self.valid_document()
        document["artifact_manifest_id"] = "other-manifest"
        self.write_document(document)
        self.assert_wire_error("invalid_manifest")

        document = self.valid_document()
        document["observations"] *= 257
        self.write_document(document)
        self.assert_wire_error("input_limit")

    def test_anchor_span_limits_and_source_bounds_fail_closed(self):
        document = self.copied_document()
        anchor = document["observations"][0]["anchors"][0]
        anchor["end"] = anchor["start"] + 8193
        self.write_document(document)
        self.assert_wire_error("input_limit")

        document = self.valid_document()
        anchor = document["observations"][0]["anchors"][0]
        anchor["start"] = 0
        anchor["end"] = len(self.source) + 1
        self.write_document(document)
        self.assert_wire_error("invalid_anchor")

    def test_document_link_is_rejected_without_following(self):
        target = self.root / "document-target.json"
        target.write_bytes(self.document_path.read_bytes())
        self.document_path.unlink()
        self.document_path.symlink_to(target)
        self.assert_wire_error("invalid_document")

    def test_manifest_link_is_rejected_without_following(self):
        target = self.root / "manifest-target.json"
        target.write_bytes(self.manifest_path.read_bytes())
        self.manifest_path.unlink()
        self.manifest_path.symlink_to(target)
        self.assert_wire_error("invalid_manifest")

    def test_source_file_root_and_nested_directory_links_are_rejected_without_following(self):
        with self.subTest(kind="file"):
            target = self.root / "source-target.js"
            target.write_bytes(self.source)
            source = self.source_root / self.asset_path
            source.unlink()
            source.symlink_to(target)
            self.assert_wire_error("source_integrity")

        # Restore a real source tree before replacing an intermediate
        # directory.  A separately named target avoids testing a dangling
        # link rather than no-follow traversal.
        source = self.source_root / self.asset_path
        source.unlink()
        assets = source.parent
        assets.rmdir()
        linked_assets = self.root / "linked-assets"
        linked_assets.mkdir()
        (linked_assets / "auth-store.js").write_bytes(self.source)
        assets.symlink_to(linked_assets, target_is_directory=True)
        with self.subTest(kind="nested_directory"):
            self.assert_wire_error("source_integrity")

    def test_source_root_link_is_rejected_without_following(self):
        real_root = self.root / "real-source-root"
        self.source_root.rename(real_root)
        self.source_root.symlink_to(real_root, target_is_directory=True)
        self.assert_wire_error("source_integrity")

    def test_duplicate_json_members_and_catalog_semantics_are_checked_before_indexing(self):
        self.document_path.write_text(
            '{"schema_version":1,"schema_version":1,"id":"portunex.wire.identity",'
            '"artifact_manifest_id":"portunex-static-assets-v1","observations":[]}',
            encoding="utf-8",
        )
        self.assert_wire_error("invalid_document")

        self.write_document(self.valid_document())
        catalog_path = self.catalog_root / "portunex" / "identity" / "manifest.json"
        catalog = json.loads(catalog_path.read_text(encoding="utf-8"))
        catalog["discovered"]["api_paths"][0]["unknowns"] = []
        self.write_json(catalog_path, catalog)
        self.assert_wire_error("catalog_reference")

    def test_non_json_numbers_are_not_accepted_as_offsets(self):
        raw = (
            '{"schema_version":1,"id":"portunex.wire.identity",'
            '"artifact_manifest_id":"portunex-static-assets-v1","observations":['
            '{"id":"portunex.identity.login.method","record_id":"portunex.identity.api.login",'
            '"path":"/portunex/auth/login","aspect":"method","statement":"synthetic",'
            '"anchors":[{"artifact":"assets/auth-store.js","start":NaN,"end":10,'
            '"sha256":"' + "0" * 64 + '","literal":"POST"}]}]}'
        )
        self.document_path.write_text(raw, encoding="utf-8")
        self.assert_wire_error("invalid_document")

    def test_statement_is_not_semantically_promoted_and_catalog_is_read_once(self):
        document = self.copied_document()
        document["observations"][0]["statement"] = "This intentionally unproven sentence is only reviewer text."
        self.write_document(document)
        catalog_path = self.catalog_root / "portunex" / "identity" / "manifest.json"
        before = (catalog_path.read_bytes(), catalog_path.stat().st_mtime_ns)
        original_load = wire_validation.load_catalog
        with patch("recoverykit.wire.validation.load_catalog", wraps=original_load) as load:
            result = self.validate()
        after = (catalog_path.read_bytes(), catalog_path.stat().st_mtime_ns)
        self.assertEqual(1, load.call_count)
        self.assertFalse(result["business_verification"])
        self.assertEqual("source_anchored", result["status"])
        self.assertEqual(before, after)

    def test_validation_reads_static_bytes_only_and_never_executes_source(self):
        target = self.source_root / self.asset_path
        before = target.read_bytes()
        before_stat = target.stat()
        catalog_path = self.catalog_root / "portunex" / "identity" / "manifest.json"
        before_catalog = catalog_path.read_bytes()
        with patch.object(subprocess, "run") as run, patch.object(os, "system") as system:
            result = self.validate()
        self.assertEqual("source_anchored", result["status"])
        run.assert_not_called()
        system.assert_not_called()
        after_stat = target.stat()
        self.assertEqual(before, target.read_bytes())
        self.assertEqual((before_stat.st_mtime_ns, before_stat.st_size), (after_stat.st_mtime_ns, after_stat.st_size))
        self.assertEqual(before_catalog, catalog_path.read_bytes())
