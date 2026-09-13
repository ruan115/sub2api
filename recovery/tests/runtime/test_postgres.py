"""Offline source/receipt guards. Does not download or prove binary execution."""

from pathlib import Path
import re
import unittest

from recoverykit.evidence.filesystem import load_json_bytes
from recoverykit.evidence.policy import check_content

ROOT = Path(__file__).resolve().parents[2]


class PostgresRuntimeMetadataTests(unittest.TestCase):
    def test_pinned_official_source_and_explicit_test_scope(self):
        raw = (ROOT / "runtime/postgres/source.json").read_bytes()
        check_content(raw, text_only=True)
        source = load_json_bytes(raw)
        self.assertEqual(set(source), {"schema_version", "version", "archive_url", "checksum_url", "archive_bytes", "archive_sha256",
                                      "configure_flags", "extra_extension", "extension_version", "test_locale", "test_encoding",
                                      "production_build_parity_verified", "system_install", "automatic_download"})
        self.assertIs(type(source["schema_version"]), int)
        self.assertEqual(source["schema_version"], 1)
        self.assertEqual(source["version"], "18.6")
        self.assertEqual(source["archive_url"], "https://ftp.postgresql.org/pub/source/v18.6/postgresql-18.6.tar.gz")
        self.assertEqual(source["checksum_url"], source["archive_url"] + ".sha256")
        self.assertEqual(source["archive_sha256"], "983ee554ec53dbeb9b70797bef9fcf4e67e117e7e48ca1463cc80b3ff8e8ff3f")
        self.assertIs(type(source["archive_bytes"]), int)
        self.assertEqual(source["archive_bytes"], 29598283)
        self.assertEqual(source["configure_flags"], ["--without-icu", "--without-readline"])
        self.assertEqual((source["extra_extension"], source["extension_version"], source["test_locale"], source["test_encoding"]), ("citext", "1.8", "C", "UTF8"))
        for field in ("production_build_parity_verified", "system_install", "automatic_download"):
            self.assertIs(source[field], False)

    def test_local_receipt_does_not_claim_reproducible_or_production_build(self):
        raw = (ROOT / "runtime/postgres/local-build-2026-09-14.json").read_bytes()
        check_content(raw, text_only=True)
        receipt = load_json_bytes(raw)
        source = load_json_bytes((ROOT / "runtime/postgres/source.json").read_bytes())
        self.assertIs(type(receipt["schema_version"]), int)
        self.assertEqual(receipt["schema_version"], 1)
        self.assertEqual(receipt["kind"], "portunex.synthetic_postgres.local_build")
        self.assertEqual(receipt["source_sha256"], source["archive_sha256"])
        self.assertEqual(receipt["postgres_version"], source["version"])
        self.assertEqual(receipt["configure_flags"], source["configure_flags"])
        for field in ("system_software_replaced", "production_database_accessed", "original_legacy_server_executed", "reproducible_binary_build_verified"):
            self.assertIs(receipt[field], False)
        paths = set()
        for item in receipt["files"]:
            self.assertEqual(set(item), {"path", "bytes", "sha256"})
            self.assertRegex(item["path"], r"^(bin|lib|share/extension)/[a-z0-9_.]+$")
            self.assertNotIn(item["path"], paths)
            paths.add(item["path"])
            self.assertIs(type(item["bytes"]), int)
            self.assertGreater(item["bytes"], 0)
            self.assertRegex(item["sha256"], r"^[0-9a-f]{64}$")
        self.assertEqual(paths, {"bin/postgres", "bin/initdb", "bin/psql", "bin/pg_config", "lib/citext.dylib", "share/extension/citext.control"})

    def test_database_gate_is_opt_in(self):
        makefile = (ROOT / "Makefile").read_text()
        for target in ("check", "check-demo", "test", "backend-demo"):
            dependencies = re.search(r"^" + target + r":([^\n]*)$", makefile, re.M).group(1)
            self.assertNotIn("postgres-integration", dependencies)
        recipe = re.search(r"^postgres-integration:\n((?:\t[^\n]*\n)+)", makefile, re.M).group(1)
        self.assertIn("-tags portunex_integration", recipe)
        self.assertNotIn("curl", recipe)
        self.assertNotIn("docker", recipe)
