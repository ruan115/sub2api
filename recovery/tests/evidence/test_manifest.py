from __future__ import annotations

import copy
import json
import os

from recoverykit.evidence import EvidenceError, load_manifest, verify_manifest
from .support import EvidenceFixture


class ManifestTests(EvidenceFixture):
    def test_load_and_verify_manifest(self):
        manifest = self.manifest([self.entry(), self.entry("Dockerfile.vm", b"FROM scratch\n")])
        path = self.root / "input.json"
        path.write_text(json.dumps(manifest))
        self.assertEqual(load_manifest(path), manifest)
        report = verify_manifest(manifest, self.source)
        self.assertEqual(report["status"], "verified")
        self.assertEqual(report["file_count"], 2)
        json.dumps(report)

    def test_invalid_paths_are_rejected_before_access(self):
        valid = self.manifest()
        for path in ["/etc/passwd", "../other.proto", "a/../../other.proto", "./a.proto",
                     "a//b.proto", "a/../b.proto", "C:/a.proto", "C:\\a.proto", "a\n.proto", "a/"]:
            with self.subTest(path=path):
                manifest = copy.deepcopy(valid)
                manifest["entries"][0]["path"] = path
                with self.assertRaises(EvidenceError):
                    verify_manifest(manifest, self.source)

    def test_schema_unknown_missing_duplicates_and_invalid_numbers(self):
        valid = self.manifest()
        variants = []
        for field in ["schema_version", "id", "entries"]:
            changed = copy.deepcopy(valid)
            del changed[field]
            variants.append(changed)
        variants += [dict(valid, unexpected=True), dict(valid, schema_version=True), dict(valid, entries=[])]
        for field, value in [("size", True), ("size", -1), ("sha256", "f" * 63),
                             ("source", ""), ("source", None)]:
            changed = copy.deepcopy(valid)
            changed["entries"][0][field] = value
            variants.append(changed)
        variants.append(dict(valid, entries=valid["entries"] * 2))
        for variant in variants:
            with self.subTest(variant=variant):
                with self.assertRaises(EvidenceError):
                    verify_manifest(variant, self.source)

    def test_case_and_prefix_collisions(self):
        entries = [self.entry("a.proto"), self.entry("A.proto")]
        # Explicit dict construction also covers case-insensitive filesystems.
        for paths in [("a.proto", "A.proto"), ("a.proto", "a.proto/nested.proto")]:
            variants = copy.deepcopy(entries)
            for entry, path in zip(variants, paths):
                entry["path"] = path
            with self.assertRaises(EvidenceError):
                verify_manifest(self.manifest(variants), self.source)

    def test_duplicate_json_member_rejected(self):
        path = self.root / "bad.json"
        path.write_text('{"id":"one","id":"two","schema_version":1,"entries":[]}')
        with self.assertRaises(EvidenceError):
            load_manifest(path)

    def test_manifest_id_cannot_carry_a_recognisable_secret(self):
        manifest = self.manifest()
        manifest["id"] = "ghp_" + "A" * 40
        with self.assertRaises(EvidenceError):
            verify_manifest(manifest, self.source)

    def test_sensitive_opaque_and_binary_inputs_rejected(self):
        for name in [".env", ".env.example", "credentials.json", "key.pem", "server.crt",
                     "backup.zip", "backup.tar", "db.sqlite", "logs.log", "payload.pkg", "program"]:
            with self.subTest(name=name):
                with self.assertRaises(EvidenceError):
                    verify_manifest(self.manifest([self.entry(name, b"fixture")]), self.source)
        for data in [b"\x7fELF\x00binary", b"bad utf8 \xff",
                     b"sk-ant-api03-" + b"A" * 40,
                     b"-----BEGIN PRIVATE KEY-----\n" + b"A" * 64,
                     b"socks5://" + b"alice:real-password@proxy.invalid"]:
            with self.subTest(kind=data[:5]):
                with self.assertRaises(EvidenceError):
                    verify_manifest(self.manifest([self.entry("source.js", data)]), self.source)

    def test_code_with_auth_names_and_key_header_constants_is_allowed(self):
        entry = self.entry("bin/lib/auth.sh", b'printf "%s" "-----BEGIN PRIVATE KEY-----"\n')
        self.assertEqual(verify_manifest(self.manifest([entry]), self.source)["file_count"], 1)

    def test_missing_tampered_and_declared_wrong_size_fail(self):
        manifest = self.manifest()
        (self.source / "code/test.proto").write_bytes(b"changed")
        with self.assertRaises(EvidenceError):
            verify_manifest(manifest, self.source)
        (self.source / "code/test.proto").unlink()
        with self.assertRaises(EvidenceError):
            verify_manifest(manifest, self.source)

    def test_file_directory_root_and_manifest_symlinks_fail(self):
        manifest = self.manifest()
        original = self.source / "code/test.proto"
        outside = self.root / "outside.proto"
        outside.write_bytes(original.read_bytes())
        original.unlink()
        original.symlink_to(outside)
        with self.assertRaises(EvidenceError):
            verify_manifest(manifest, self.source)
        original.unlink()
        (self.source / "code").rmdir()
        directory = self.root / "directory"
        directory.mkdir()
        (directory / "test.proto").write_bytes(outside.read_bytes())
        (self.source / "code").symlink_to(directory, target_is_directory=True)
        with self.assertRaises(EvidenceError):
            verify_manifest(manifest, self.source)
        link = self.root / "linked-source"
        link.symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(EvidenceError):
            verify_manifest(manifest, link)
        metadata = self.root / "manifest.json"
        metadata.symlink_to(outside)
        with self.assertRaises(EvidenceError):
            load_manifest(metadata)

    def test_fifo_is_rejected_without_blocking(self):
        if not hasattr(os, "mkfifo"):
            self.skipTest("POSIX FIFO required")
        entry = self.entry("fifo.proto", b"")
        path = self.source / entry["path"]
        path.unlink()
        os.mkfifo(path)
        with self.assertRaises(EvidenceError):
            verify_manifest(self.manifest([entry]), self.source)
