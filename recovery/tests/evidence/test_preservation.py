from __future__ import annotations

import json
import os
import stat
from unittest.mock import patch

from recoverykit.evidence import EvidenceError, preserve_manifest, verify_preserved
from recoverykit.evidence import preservation
from recoverykit.evidence.manifest import validate_manifest
from .support import EvidenceFixture


class PreservationTests(EvidenceFixture):
    def test_roundtrip_private_files_and_source_unchanged(self):
        manifest = self.manifest()
        source = self.source / manifest["entries"][0]["path"]
        source.chmod(0o755)
        before = (source.read_bytes(), source.stat().st_mode, source.stat().st_mtime_ns)
        result = preserve_manifest(manifest, self.source, self.destination)
        self.assertEqual(result, verify_preserved(self.destination))
        self.assertEqual(result["file_count"], 1)
        self.assertEqual(before, (source.read_bytes(), source.stat().st_mode, source.stat().st_mtime_ns))
        for root, directories, files in os.walk(self.destination):
            self.assertEqual(stat.S_IMODE(os.stat(root).st_mode), 0o700)
            for name in files:
                self.assertEqual(stat.S_IMODE(os.stat(os.path.join(root, name)).st_mode), 0o600)
        json.dumps(result)

    def test_preflight_failure_creates_no_destination(self):
        manifest = self.manifest([self.entry(), self.entry("bad.proto", b"test")])
        (self.source / "bad.proto").write_bytes(b"tampered")
        with self.assertRaises(EvidenceError):
            preserve_manifest(manifest, self.source, self.destination)
        self.assertFalse(self.destination.exists())

    def test_canonical_metadata_size_is_checked_before_destination_creation(self):
        empty = self.entry("empty.txt", b"")
        entries = [dict(empty, path="%04d.txt" % i, source="x" * 100) for i in range(4096)]
        manifest = self.manifest(entries)
        self.assertLess(len(json.dumps(manifest, separators=(",", ":")).encode()), 1024 * 1024)
        with self.assertRaisesRegex(EvidenceError, "canonical manifest exceeds metadata size limit"):
            validate_manifest(manifest)
        with self.assertRaisesRegex(EvidenceError, "canonical manifest exceeds metadata size limit"):
            preserve_manifest(manifest, self.source, self.destination)
        self.assertFalse(self.destination.exists())

    def test_replay_refuses_existing_destination_without_overwriting(self):
        manifest = self.manifest()
        preserve_manifest(manifest, self.source, self.destination)
        before = (self.destination / "receipt.json").read_bytes()
        with self.assertRaises(EvidenceError):
            preserve_manifest(manifest, self.source, self.destination)
        self.assertEqual(before, (self.destination / "receipt.json").read_bytes())
        verify_preserved(self.destination)

    def test_relative_source_nested_git_and_symlink_destinations_fail(self):
        manifest = self.manifest()
        gitroot = self.root / "gitroot"
        gitroot.mkdir()
        (gitroot / ".git").mkdir()
        link = self.root / "link"
        link.symlink_to(self.root, target_is_directory=True)
        variants = ["relative-output", self.source / "nested", gitroot / "private", link / "private"]
        for target in variants:
            with self.subTest(target=str(target)):
                with self.assertRaises(EvidenceError):
                    preserve_manifest(manifest, self.source, target)

    def test_changed_between_preflight_and_copy_is_not_complete(self):
        manifest = self.manifest()
        original = preservation.read_entry

        def changed(entry, root):
            (root / entry["path"]).write_bytes(b"changed after preflight")
            return original(entry, root)

        with patch.object(preservation, "read_entry", side_effect=changed):
            with self.assertRaises(EvidenceError):
                preserve_manifest(manifest, self.source, self.destination)
        self.assertTrue(self.destination.is_dir())
        self.assertFalse((self.destination / "receipt.json").exists())
        with self.assertRaises(EvidenceError):
            verify_preserved(self.destination)

    def test_write_failure_leaves_no_valid_receipt(self):
        manifest = self.manifest()
        original = preservation.write_exclusive

        def fail_manifest(root, name, data):
            if name == "manifest.json":
                raise EvidenceError("synthetic write failure")
            original(root, name, data)

        with patch.object(preservation, "write_exclusive", side_effect=fail_manifest):
            with self.assertRaises(EvidenceError):
                preserve_manifest(manifest, self.source, self.destination)
        with self.assertRaises(EvidenceError):
            verify_preserved(self.destination)

    def test_tampering_extra_file_permissions_and_symlink_are_detected(self):
        manifest = self.manifest()
        for case in ["content", "manifest", "extra", "permissions", "symlink"]:
            with self.subTest(case=case):
                destination = self.root / case
                preserve_manifest(manifest, self.source, destination)
                target = destination / "files/code/test.proto"
                if case == "content":
                    target.write_bytes(b"altered")
                elif case == "manifest":
                    (destination / "manifest.json").write_text("{}")
                elif case == "extra":
                    extra = destination / "unexpected.txt"
                    extra.write_text("unexpected")
                    extra.chmod(0o600)
                elif case == "permissions":
                    target.chmod(0o644)
                else:
                    target.unlink()
                    target.symlink_to(self.source / "code/test.proto")
                with self.assertRaises(EvidenceError):
                    verify_preserved(destination)

    def test_verification_needs_only_preserved_copy(self):
        manifest = self.manifest()
        preserve_manifest(manifest, self.source, self.destination)
        (self.source / "code/test.proto").unlink()
        self.assertEqual(verify_preserved(self.destination)["status"], "verified")
