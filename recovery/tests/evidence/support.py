from __future__ import annotations

import hashlib
from pathlib import Path
import tempfile
import unittest


class EvidenceFixture(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.destination = self.root / "preserved"

    def entry(self, path="code/test.proto", data=b'syntax = "proto3";\n'):
        target = self.source / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        return {"path": path, "size": len(data), "sha256": hashlib.sha256(data).hexdigest(),
                "source": "synthetic offline test fixture"}

    def manifest(self, entries=None):
        return {"schema_version": 1, "id": "test-baseline", "entries": entries or [self.entry()]}
