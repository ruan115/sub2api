"""Synthetic-only tests of the strictly pinned visual-preview derivation."""

import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import json_bytes, write_exclusive


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("provider_preview", ROOT / "preview/frontend/prepare_provider.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ProviderPreviewTests(unittest.TestCase):
    def test_original_metadata_is_pinned_and_no_approval_for_production(self):
        doc = MODULE.receipt_document()
        self.assertEqual(doc["source_bytes"], 96456)
        self.assertEqual(doc["artifact_bytes"], 96445)
        self.assertEqual(len(doc["changes"]), 2)
        self.assertFalse(doc["original_quarantine_modified"])
        self.assertFalse(doc["production_approved"])
        self.assertFalse(doc["business_compatibility_verified"])
        with self.assertRaises(EvidenceError):
            MODULE.sanitize(b"unreviewed input")

    def test_exact_two_literal_replacement_and_preservation(self):
        raw = b'({placeholder:"first",other:1,placeholder:"second"})'
        first, second = b'"first"', b'"second"'
        ranges = tuple((raw.index(v), raw.index(v) + len(v), MODULE.digest(v)) for v in (first, second))
        expected = raw.replace(first, MODULE.REPLACEMENT).replace(second, MODULE.REPLACEMENT)
        values = dict(SOURCE_SIZE=len(raw), SOURCE_SHA=MODULE.digest(raw), PATCHES=ranges,
                      PREVIEW_SIZE=len(expected), PREVIEW_SHA=MODULE.digest(expected))
        with patch.multiple(MODULE, **values), tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve()) as tmp:
            root = Path(tmp)
            source = root / "source"
            source.mkdir(mode=0o700)
            write_exclusive(source, MODULE.SOURCE_PATH, raw)
            target = root / "preview"
            MODULE.prepare(source, target)
            self.assertEqual(MODULE.load_prepared(target), expected)
            self.assertEqual((source / MODULE.SOURCE_PATH).read_bytes(), raw)
            with self.assertRaises(EvidenceError):
                MODULE.prepare(source, target)
            path = target / MODULE.ARTIFACT_PATH
            path.write_bytes(b"corrupt")
            with self.assertRaises(EvidenceError):
                MODULE.load_prepared(target)
            path.write_bytes(expected)
            receipt = MODULE.receipt_document()
            receipt["production_approved"] = True
            (target / "receipt.json").write_bytes(json_bytes(receipt))
            with self.assertRaises(EvidenceError):
                MODULE.load_prepared(target)

    def test_wrong_literal_or_candidate_digest_rejected(self):
        raw = b'"synthetic"'
        with patch.multiple(MODULE, SOURCE_SIZE=len(raw), SOURCE_SHA=MODULE.digest(raw),
                            PATCHES=((0, len(raw), "0" * 64),)):
            with self.assertRaises(EvidenceError):
                MODULE.sanitize(raw)


if __name__ == "__main__":
    unittest.main()
