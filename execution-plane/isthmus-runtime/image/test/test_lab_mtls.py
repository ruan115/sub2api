"""Synthetic payload/engine only; never runs Docker or a test binary."""
import hashlib
import json
import os
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest import TestCase
from unittest.mock import Mock, patch

from lab import mtls
from test.test_lab_toolchain import fixture


class MTLSInputTests(TestCase):
    def files(self, root, names=mtls.NAMES):
        (root / "mtls-bin").mkdir(mode=0o700)
        content = b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\0" + b"synthetic-not-executable"
        records = []
        for name in names:
            (root / "mtls-bin" / name).write_bytes(content)
            records.append({"name": name, "size": len(content), "sha256": hashlib.sha256(content).hexdigest()})
        (root / "mtls-binaries.json").write_text(json.dumps(records))
        return records

    def test_exact_three_binaries(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            records = self.files(root)
            self.assertEqual(mtls.inputs(SimpleNamespace(root=root)),
                [(root / "mtls-bin" / name, "bin/" + name, record, 0o755)
                 for name, record in zip(mtls.NAMES, records)])

    def test_enrollment_profile_is_separate_and_explicit(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            self.files(root, mtls.ENROLLMENT_NAMES)
            self.assertEqual(len(mtls.inputs(SimpleNamespace(root=root), mtls.ENROLLMENT_NAMES)), 9)
            with self.assertRaises(ValueError):
                mtls.inputs(SimpleNamespace(root=root))
            self.assertEqual(len(mtls.ENROLLMENT_NAMES), len(mtls.ENROLLMENT_RUNS))
            self.assertIn("^TestRuntimeEnrollment", mtls.ENROLLMENT_RUNS)
        with self.assertRaises(ValueError):
            mtls.probe(None, enrollment="yes")

    def test_rejects_unbounded_untrusted_or_changed_payload(self):
        for mode in ("name", "size", "bool-size", "extra", "hash", "elf", "link", "symlink", "duplicate"):
            with self.subTest(mode=mode), TemporaryDirectory() as tmp:
                root = Path(tmp).resolve()
                records = self.files(root)
                path = root / "mtls-bin" / mtls.NAMES[0]
                if mode == "name": records[0]["name"] = "other;binary"
                elif mode == "size": records[0]["size"] = 64 * 1024**2 + 1
                elif mode == "bool-size": records[0]["size"] = True
                elif mode == "extra": records[0]["unexpected"] = True
                elif mode == "hash": records[0]["sha256"] = "a" * 64
                elif mode == "elf": path.write_bytes(b"x" + path.read_bytes()[1:])
                elif mode == "link": os.link(path, root / "other")
                elif mode == "symlink": path.rename(root / "other"); path.symlink_to(root / "other")
                raw = json.dumps(records)
                if mode == "duplicate": raw = raw.replace('"name":', '"name":"duplicate","name":', 1)
                (root / "mtls-binaries.json").write_text(raw)
                with self.assertRaises((ValueError, OSError)):
                    mtls.inputs(SimpleNamespace(root=root))

    def test_cleanup_success_failure_and_uncertain_create(self):
        for mode in ("pass", "test-fail", "uncertain", "empty-tests"):
            with self.subTest(mode=mode), TemporaryDirectory() as tmp:
                root = Path(tmp).resolve()
                self.files(root)
                lab = Mock(root=root, config=root / "config", prefix=["synthetic-docker"])
                lab.name = "isthmus-s1b-synthetic"
                lab.baseline.return_value = ["unchanged"]
                state = fixture()
                state["HostConfig"]["Memory"] = state["HostConfig"]["MemorySwap"] = 1024**3
                state["State"] = {"Running": True, "OOMKilled": False}
                lab.owned.return_value = state
                def run(*args, **kwargs):
                    if args[0] == "create":
                        if mode == "uncertain": raise TimeoutError("uncertain")
                        return SimpleNamespace(stdout="a" * 64)
                    return SimpleNamespace(stdout="")
                lab.run.side_effect = run
                def logged(name, args, **kwargs):
                    self.assertEqual(args[1:3], ["--user", "1000:1000"])
                    self.assertIn("-test.count=3", args[-1])
                    self.assertNotIn("PRIVATE", args[-1])
                    if mode == "test-fail": raise ValueError("test-failure")
                    warning = "testing: warning: no tests to run\n" if mode == "empty-tests" else ""
                    (root / (name + ".log")).write_text(warning + "synthetic-mtls-native-pass\n")
                lab.logged.side_effect = logged
                original = Path.read_text
                def read(path, *args, **kwargs):
                    return "MemAvailable: 4194304 kB\n" if str(path) == "/proc/meminfo" else original(path, *args, **kwargs)
                with patch.object(Path, "read_text", read), patch.object(mtls, "bounded_process", return_value=SimpleNamespace(returncode=0, stdout="root-upload-capless-pass\n")):
                    if mode == "pass": mtls.probe(lab)
                    else:
                        with self.assertRaises((ValueError, TimeoutError)): mtls.probe(lab)
                receipts = {call.args[0]: call.args[1] for call in lab.save.call_args_list}
                self.assertEqual(receipts["identity-cleanup.json"]["cleanup_complete"], mode != "uncertain")
                self.assertEqual("mtls-result.json" in receipts, mode == "pass")
                removed = [call.args for call in lab.run.call_args_list if call.args[0] == "rm"]
                self.assertEqual(removed, [] if mode == "uncertain" else [("rm", "a" * 64)])
