"""Private synthetic fixtures for wire-evidence tests; never production inputs."""

from __future__ import annotations

import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest


class WireFixtureCase(unittest.TestCase):
    def setUp(self):
        # The evidence no-follow helper deliberately rejects `/var` symlink
        # ancestry on macOS.  Use the physical system temp root so this test
        # exercises the same safe descriptor path used for private artifacts.
        self._temporary_directory = tempfile.TemporaryDirectory(dir="/private/tmp")
        self.root = Path(self._temporary_directory.name)
        self.source_root = self.root / "private-source"
        self.source_root.mkdir()
        self.asset_path = "assets/auth-store.js"
        self.source = (
            'const call = fetch("/portunex/auth/login", { method: "POST", credentials: "include" });\n'
            'const label = "你好";\n'
        ).encode("utf-8")
        target = self.source_root / self.asset_path
        target.parent.mkdir(parents=True)
        target.write_bytes(self.source)
        self.manifest_path = self.root / "artifacts.json"
        self.manifest = {
            "schema_version": 1,
            "id": "portunex-static-assets-v1",
            "entries": [
                {
                    "path": self.asset_path,
                    "size": len(self.source),
                    "sha256": hashlib.sha256(self.source).hexdigest(),
                    "source": "synthetic reviewed static source",
                }
            ],
        }
        self.write_json(self.manifest_path, self.manifest)
        self.catalog_root = self.root / "catalog"
        self._write_catalog()
        self.document_path = self.root / "observations.json"
        self.document = self.valid_document()
        self.write_json(self.document_path, self.document)

    def tearDown(self):
        self._temporary_directory.cleanup()

    def _write_catalog(self):
        document = {
            "schema_version": 1,
            "id": "portunex.identity",
            "owner": "portunex",
            "status": "discovered",
            "evidence": [
                {
                    "id": "static_asset",
                    "kind": "static_asset",
                    "source": "synthetic fixture",
                    "observed": "A literal path was present.",
                }
            ],
            "discovered": {
                "api_paths": [
                    {
                        "id": "portunex.identity.api.login",
                        "path": "/portunex/auth/login",
                        "method": "unknown",
                        "status": "discovered",
                        "evidence": ["static_asset"],
                        "unknowns": ["HTTP behavior remains unverified."],
                    }
                ]
            },
            "unknowns": ["This is not a complete API contract."],
        }
        path = self.catalog_root / "portunex" / "identity" / "manifest.json"
        path.parent.mkdir(parents=True)
        self.write_json(path, document)

    def anchor(self, literal="POST", start=None, end=None):
        if start is None:
            start = self.source.index(b"fetch")
        if end is None:
            end = self.source.index(b"\n")
        span = self.source[start:end]
        return {
            "artifact": self.asset_path,
            "start": start,
            "end": end,
            "sha256": hashlib.sha256(span).hexdigest(),
            "literal": literal,
        }

    def valid_document(self):
        return {
            "schema_version": 1,
            "id": "portunex.wire.identity",
            "artifact_manifest_id": self.manifest["id"],
            "observations": [
                {
                    "id": "portunex.identity.login.method",
                    "record_id": "portunex.identity.api.login",
                    "path": "/portunex/auth/login",
                    "aspect": "method",
                    "statement": "The reviewed client source contains a literal method token.",
                    "anchors": [self.anchor()],
                }
            ],
        }

    def write_document(self, document):
        self.document = document
        self.write_json(self.document_path, document)

    def write_json(self, path, value):
        path.write_text(json.dumps(value, ensure_ascii=False), encoding="utf-8")

    def copied_document(self):
        return copy.deepcopy(self.document)
