"""Committed metadata checks only; original assets and server are never needed."""

import hashlib
import importlib.util
from pathlib import Path
import unittest

from recoverykit.evidence.filesystem import json_bytes, load_json_bytes
from recoverykit.evidence.policy import check_content


ROOT = Path(__file__).resolve().parents[2]
BASE = ROOT / "baselines/portunex/frontend"
SPEC = importlib.util.spec_from_file_location("frontend_capture_baseline", ROOT / "collectors/frontend/capture.py")
CAPTURE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CAPTURE)


class FrontendBaselineTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        raw = (BASE / "preservation-2026-09-14.json").read_bytes()
        check_content(raw, text_only=True)
        cls.baseline = load_json_bytes(raw)
        raw = (BASE / "theme-observations.json").read_bytes()
        check_content(raw, text_only=True)
        cls.themes = load_json_bytes(raw)

    def test_exact_recorded_scope_and_inventory_digest(self):
        doc = self.baseline
        self.assertEqual(doc["kind"], "ccmax.legacy_frontend.static_preservation")
        self.assertEqual(doc["source"], CAPTURE.SOURCE)
        self.assertEqual(doc["file_count"], 148)
        self.assertEqual(len(doc["entries"]), doc["file_count"])
        self.assertEqual(doc["total_bytes"], 4666514)
        self.assertEqual(sum(e["bytes"] for e in doc["entries"]), doc["total_bytes"])
        inventory = {
            "schema_version": 1, "kind": "portunex.frontend.inventory", "source": doc["source"],
            "entries": [{k: entry[k] for k in ("path", "bytes", "sha256")} for entry in doc["entries"]],
        }
        CAPTURE.validate_inventory(inventory)
        self.assertEqual(hashlib.sha256(json_bytes(inventory)).hexdigest(), doc["inventory_sha256"])
        for field in ("collector_sha256", "receipt_sha256"):
            self.assertRegex(doc[field], r"^[0-9a-f]{64}$")

    def test_preservation_is_not_execution_or_publication(self):
        for flag in ("source_executed", "business_rows_read", "approved_for_execution",
                     "approved_for_publication", "original_source_project_recovered"):
            self.assertIs(self.baseline[flag], False)
        entries = self.baseline["entries"]
        self.assertEqual({c: sum(e["category"] == c for e in entries)
                          for c in ("files", "quarantine", "reference")},
                         {"files": 138, "quarantine": 1, "reference": 9})
        self.assertEqual([(e["path"], e["reason"]) for e in entries if e["category"] == "quarantine"],
                         [("assets/_dashboard.providers-ABk0JmwU.js", "recognisable_secret_material")])
        self.assertEqual(sum(e["path"].endswith(".js") for e in entries), 135)
        self.assertEqual(sum(e["path"].endswith(".css") for e in entries), 2)

    def test_previously_preserved_javascript_unchanged(self):
        old = load_json_bytes((ROOT / "baselines/portunex/static-wire-artifacts.json").read_bytes())
        entries = {e["path"]: e for e in self.baseline["entries"]}
        self.assertEqual(len(old["entries"]), 11)
        for old_entry in old["entries"]:
            current = entries["assets/" + old_entry["path"]]
            self.assertEqual((current["bytes"], current["sha256"]), (old_entry["size"], old_entry["sha256"]))

    def test_theme_anchors_have_source_and_keep_cascade_conditions(self):
        doc = self.themes
        self.assertIs(doc["visual_parity_verified"], False)
        self.assertIs(doc["original_javascript_executed"], False)
        css = next(e for e in self.baseline["entries"] if e["path"] == doc["source"]["path"])
        self.assertEqual(doc["source"], {k: css[k] for k in ("path", "bytes", "sha256")})
        self.assertEqual(len(doc["declarations"]), 22)
        previous = -1
        for record in doc["declarations"]:
            self.assertIs(type(record["start_byte"]), int)
            self.assertIs(type(record["end_byte"]), int)
            self.assertGreater(record["start_byte"], previous)
            self.assertLess(record["start_byte"], record["end_byte"])
            self.assertLessEqual(record["end_byte"], css["bytes"])
            self.assertRegex(record["sha256"], r"^[0-9a-f]{64}$")
            self.assertIn(record["selector"], (":root,:host", ":root", ".dark"))
            self.assertTrue(set(record["conditions"]) <= {"@layer theme", "@supports (color: oklab(0% 0 0%))"})
            previous = record["start_byte"]
        self.assertTrue(any(d["selector"] == ".dark" and d["conditions"] for d in doc["declarations"]))


if __name__ == "__main__":
    unittest.main()
