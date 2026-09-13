"""Offline synthetic extraction tests; no ELF or SSH needed."""

import hashlib
import importlib.util
import copy
import json
from pathlib import Path
import unittest

from recoverykit.evidence.policy import check_content
from recoverykit.evidence.filesystem import load_json_bytes
from recoverykit.evidence import EvidenceError

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("identity_literals", ROOT / "collectors/binary/identity_literals.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class LiteralTests(unittest.TestCase):
    def test_exact_offsets_and_clipped_ascii(self):
        data = b"\x00abcdef\x80x\x00SELECT\n1\x00"
        windows = MODULE.excerpts(data, [(3, 14)])
        self.assertEqual(windows[0]["start"], 3)
        segments = windows[0]["segments"]
        self.assertEqual([s["text"] for s in segments], ["cdef", "SELECT\n"])
        for segment in segments:
            raw = data[segment["start"]:segment["end"]]
            self.assertEqual(segment["text"].encode("ascii"), raw)
            self.assertEqual(segment["sha256"], hashlib.sha256(raw).hexdigest())

    def test_budgets(self):
        for windows in [[(-1, 5)], [(0, 0)], [(0, 8193)], [(0, 900)], [(0, 1)] * 33]:
            with self.subTest(windows=windows), self.assertRaises(ValueError):
                MODULE.excerpts(b"data", windows)
        with self.assertRaises(ValueError):
            MODULE.excerpts(b"x" * 9000, [(0, 8192)] * 9)

    def test_marker_counts_not_behavior(self):
        result = MODULE.marker_scan(b"argon2\x00" * 20)
        found = next(item for item in result if item["marker"] == "argon2")
        self.assertEqual(found["count"], 20)
        self.assertEqual(found["first_offsets"], list(range(0, 84, 7)))
        self.assertEqual(next(item for item in result if item["marker"] == "bcrypt")["count"], 0)

    def test_fixed_source_bounds(self):
        self.assertEqual(MODULE.SOURCE, "/opt/gateway/bin/portunex-server")
        self.assertEqual(len(MODULE.SHA256), 64)
        self.assertLessEqual(sum(size for _, size in MODULE.WINDOWS), 65536)
        self.assertTrue(all(start >= 0 and 0 < size <= 8192 and start + size <= MODULE.SIZE
                            for start, size in MODULE.WINDOWS))


class LiteralArtifactTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.artifact_bytes = (ROOT / "baselines/portunex/binary/identity-literals.json").read_bytes()
        cls.doc = load_json_bytes(cls.artifact_bytes)
        cls.provenance = load_json_bytes((ROOT / "baselines/portunex/binary/provenance.json").read_bytes())

    def assertArtifact(self, doc):
        self.assertEqual(set(doc), {"schema_version", "kind", "source", "runtime_verified",
                                   "business_rows_read", "extraction", "markers", "windows"})
        self.assertIs(type(doc["schema_version"]), int)
        self.assertEqual(doc["schema_version"], 1)
        self.assertEqual(doc["kind"], "portunex.identity.static_literals")
        self.assertEqual(doc["source"], {"path": MODULE.SOURCE, "size": MODULE.SIZE, "sha256": MODULE.SHA256})
        self.assertIs(type(doc["source"]["size"]), int)
        self.assertIs(doc["runtime_verified"], False)
        self.assertIs(doc["business_rows_read"], False)
        self.assertEqual(doc["extraction"], "clipped ASCII runs (tab/LF/CR/0x20-0x7e), minimum 4 bytes")
        self.assertEqual([m["marker"] for m in doc["markers"]], list(MODULE.MARKERS))
        for marker in doc["markers"]:
            self.assertEqual(set(marker), {"marker", "count", "first_offsets"})
            self.assertIs(type(marker["count"]), int)
            self.assertGreaterEqual(marker["count"], 0)
            offsets = marker["first_offsets"]
            self.assertEqual(len(offsets), min(marker["count"], 12))
            self.assertEqual(offsets, sorted(set(offsets)))
            for offset in offsets:
                self.assertIs(type(offset), int)
                self.assertGreaterEqual(offset, 0)
                self.assertLessEqual(offset + len(marker["marker"]), MODULE.SIZE)
        self.assertEqual([(w["start"], w["end"] - w["start"]) for w in doc["windows"]], list(MODULE.WINDOWS))
        for window in doc["windows"]:
            self.assertEqual(set(window), {"start", "end", "segments"})
            self.assertIs(type(window["start"]), int)
            self.assertIs(type(window["end"]), int)
            last = window["start"]
            for segment in window["segments"]:
                self.assertEqual(set(segment), {"start", "end", "sha256", "text"})
                self.assertIs(type(segment["start"]), int)
                self.assertIs(type(segment["end"]), int)
                self.assertGreaterEqual(segment["start"], last)
                self.assertLessEqual(segment["end"], window["end"])
                raw = segment["text"].encode("ascii")
                self.assertGreaterEqual(len(raw), 4)
                self.assertEqual(len(raw), segment["end"] - segment["start"])
                self.assertTrue(all(b in (9, 10, 13) or 32 <= b <= 126 for b in raw))
                self.assertEqual(hashlib.sha256(raw).hexdigest(), segment["sha256"])
                last = segment["end"]

    def test_retained_excerpts(self):
        self.assertArtifact(self.doc)
        self.assertEqual(len(self.doc["windows"]), 8)
        self.assertEqual(sum(w["end"] - w["start"] for w in self.doc["windows"]), 2611)
        self.assertEqual(sum(len(w["segments"]) for w in self.doc["windows"]), 8)

    def assertProvenance(self, receipt):
        self.assertEqual(set(receipt), {"schema_version", "kind", "source", "collector_path", "collector_sha256",
                                       "artifact_path", "artifact_sha256", "representation", "content_screening",
                                       "source_executed", "process_memory_read", "business_rows_read", "original_elf_retained",
                                       "runtime_verified", "authentication_compatibility_verified"})
        self.assertIs(type(receipt["schema_version"]), int)
        self.assertEqual(receipt["schema_version"], 1)
        self.assertEqual(receipt["kind"], "portunex.identity.static_literal_provenance")
        for field in ("source", "representation", "content_screening"):
            self.assertIs(type(receipt[field]), str)
            self.assertTrue(receipt[field])
            self.assertLessEqual(len(receipt[field]), 8192)
        self.assertEqual(receipt["collector_path"], "recovery/collectors/binary/identity_literals.py")
        self.assertEqual(receipt["artifact_path"], "recovery/baselines/portunex/binary/identity-literals.json")
        self.assertEqual(receipt["collector_sha256"], hashlib.sha256((ROOT / "collectors/binary/identity_literals.py").read_bytes()).hexdigest())
        self.assertEqual(receipt["artifact_sha256"], hashlib.sha256(self.artifact_bytes).hexdigest())
        for field in ("source_executed", "process_memory_read", "business_rows_read", "original_elf_retained",
                      "runtime_verified", "authentication_compatibility_verified"):
            self.assertIs(receipt[field], False)
        check_content(self.artifact_bytes, text_only=True)
        check_content(json.dumps(receipt).encode(), text_only=True)

    def test_provenance_and_screening(self):
        self.assertProvenance(self.provenance)

    def test_strict_metadata_loader_and_types(self):
        for raw in (b'{"schema_version":1,"schema_version":1}', b'{"source":{"size":1,"size":2}}'):
            with self.subTest(raw=raw), self.assertRaises(EvidenceError):
                load_json_bytes(raw)
        for base, validator in ((self.doc, self.assertArtifact), (self.provenance, self.assertProvenance)):
            doc = copy.deepcopy(base)
            doc["schema_version"] = True
            with self.assertRaises(AssertionError):
                validator(doc)
        receipt = copy.deepcopy(self.provenance)
        receipt["unreviewed"] = True
        with self.assertRaises(AssertionError):
            self.assertProvenance(receipt)

    def test_reject_corrupted_or_promoted_evidence(self):
        for field, value in (("text", "changed"), ("start", 0), ("sha256", "0" * 64)):
            doc = copy.deepcopy(self.doc)
            doc["windows"][0]["segments"][0][field] = value
            with self.subTest(field=field), self.assertRaises(AssertionError):
                self.assertArtifact(doc)
        doc = copy.deepcopy(self.doc)
        doc["runtime_verified"] = True
        with self.assertRaises(AssertionError):
            self.assertArtifact(doc)
