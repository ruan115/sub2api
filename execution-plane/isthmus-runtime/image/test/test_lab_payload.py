import json
from pathlib import Path
import tempfile
import tarfile
import hashlib
import unittest
from unittest.mock import patch, Mock
from types import SimpleNamespace
from lab.payload import expected_records, upload_payload
from lab.toolchain import checksums


class ProbePayloadTests(unittest.TestCase):
    def test_upload_uses_only_fixed_regular_archive_and_root_system_tar_not_cli(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            (root / "payload/bin").mkdir(parents=True)
            (root / "payload/bin/synthetic").write_bytes(b"synthetic")
            record = {"path": "bin/synthetic", "size": 9, "sha256": hashlib.sha256(b"synthetic").hexdigest()}
            lab = Mock(root=root, prefix=["synthetic-docker"], config=root / "config")
            captured = {}
            def process(args, env, timeout, limit, **kwargs):
                captured["args"] = args
                self.assertEqual(timeout, 90)
                self.assertIsInstance(kwargs["input_fd"], int)
                return SimpleNamespace(returncode=0, stdout="root-upload-capless-pass\n")
            with patch("lab.payload.checksums", return_value=[record]), patch("lab.payload.bounded_process", side_effect=process):
                upload_payload(lab, "a" * 64)
            with tarfile.open(root / "probe-payload.tar") as archive:
                members = archive.getmembers()
                self.assertEqual(len(members), 1)
                self.assertTrue(members[0].isreg())
                self.assertEqual((members[0].name, members[0].uid, members[0].gid, members[0].mode),
                                 ("bin/synthetic", 0, 0, 0o755))
            self.assertEqual(captured["args"][1:5], ["exec", "--user", "0:0", "-i"])
            self.assertIn("exec /bin/tar --no-same-owner --no-same-permissions", captured["args"][-1])
            self.assertNotIn("--privileged", captured["args"])

    def test_reviewed_anchors_have_exact_split_and_no_home_or_secrets(self):
        records = expected_records()
        self.assertEqual(len(records), 40)
        self.assertEqual(len({e["path"] for e in records}), 40)
        self.assertEqual([e["path"] for e in records if e["path"].startswith("bin/")],
                         ["bin/bun-1.3.9", "bin/bun-1.4.2", "bin/claude"])
        self.assertTrue(all(e["path"].startswith(("app/", "bin/")) for e in records))

    def test_self_consistent_but_unreviewed_receipt_rejected_before_opening_payload(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            records = expected_records()
            records[0]["sha256"] = "0" * 64
            (root / "probe-payload.json").write_text(json.dumps(records))
            with self.assertRaisesRegex(ValueError, "probe_unreviewed_payload"):
                checksums(root)

    def test_extra_file_and_same_path_digest_change_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "payload").mkdir()
            records = [{"path": "synthetic", "size": 1, "sha256": "0" * 64}]
            (root / "probe-payload.json").write_text(json.dumps(records))
            (root / "payload/synthetic").write_bytes(b"a")
            with patch("lab.payload.expected_records", return_value=records), self.assertRaises(ValueError):
                checksums(root)
            (root / "payload/extra").write_bytes(b"a")
            with patch("lab.payload.expected_records", return_value=records), self.assertRaises(ValueError):
                checksums(root)
