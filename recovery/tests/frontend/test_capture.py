"""Synthetic local fixtures only: no SSH, original JS execution, or business rows."""

import base64
import copy
import importlib.util
import json
import os
from pathlib import Path
import shlex
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import json_bytes, load_json_bytes
from recoverykit.workspace.process import ProcessResult, run_bounded


ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("frontend_capture", ROOT / "collectors/frontend/capture.py")
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def envelope(files):
    return {
        "inventory": {"schema_version": 1, "kind": "portunex.frontend.inventory", "source": MODULE.SOURCE.copy(),
                      "entries": [{"path": name, "bytes": len(raw), "sha256": MODULE.digest(raw)}
                                  for name, raw in sorted(files.items())]},
        "contents": {name: base64.b64encode(raw).decode() for name, raw in files.items()},
    }


class CaptureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.destination = self.root / "private"
        self.files = {"index.html": b"<html></html>", "assets/app-a.js": b"/* synthetic code: never run */",
                      "assets/app-b.css": b"body{color:red}"}

    def preserve(self, files=None):
        return MODULE.preserve(self.destination, "capture", envelope(files or self.files))

    def test_capture_roundtrip_and_owner_only(self):
        report = self.preserve()
        self.assertEqual(report, MODULE.verify(self.destination))
        self.assertEqual(report["stored_counts"], {"files": 3, "quarantine": 0, "reference": 0})
        self.assertFalse(report["approved_for_execution"])
        self.assertFalse(report["approved_for_publication"])
        for folder, _, files in os.walk(self.destination):
            self.assertEqual(stat.S_IMODE(os.stat(folder).st_mode), 0o700)
            for name in files:
                self.assertEqual(stat.S_IMODE(os.stat(Path(folder) / name).st_mode), 0o600)
        for name, raw in self.files.items():
            self.assertEqual((self.destination / "files" / name).read_bytes(), raw)

    def test_inventory_has_no_contents(self):
        value = envelope(self.files)
        value["contents"] = {}
        report = MODULE.preserve(self.destination, "inventory", value)
        self.assertEqual(report["entry_count"], 3)
        self.assertEqual(set(p.name for p in self.destination.iterdir()), {"receipt.json", "inventory.json"})
        self.assertFalse(any(report["stored_counts"].values()))

    def test_sensitive_bytes_quarantined_not_printed_or_promoted(self):
        secret = b"https://" + b"synthetic-user:synthetic-password" + b"@localhost.example.invalid/"
        files = dict(self.files, **{"assets/Providers.js": secret})
        report = self.preserve(files)
        self.assertEqual(report["stored_counts"]["quarantine"], 1)
        self.assertEqual((self.destination / "quarantine/assets/Providers.js").read_bytes(), secret)
        for name in ("receipt.json", "inventory.json"):
            self.assertNotIn(secret, (self.destination / name).read_bytes())
        self.assertNotIn(secret.decode(), json.dumps(report))

    def test_binary_magic_reference_and_invalid_signatures_quarantine(self):
        avif = b"\x00\x00\x00\x18ftypmif1\x00\x00\x00\x00mif1avif"
        files = dict(self.files, **{"favicon.ico": b"\x00\x00\x01\x00fixture",
                                   "images/logo.png": b"\x89PNG\r\n\x1a\nfixture",
                                   "images/customer-service-qrcode.jpg": b"\xff\xd8\xfffixture",
                                   "textures/earth/earth_diffuse_2k.avif": avif,
                                   "images/logo-black.png": b"not an image"})
        report = self.preserve(files)
        self.assertEqual(report["stored_counts"], {"files": 3, "reference": 4, "quarantine": 1})
        self.assertEqual(MODULE.classify("logo.svg", b"<svg/>"),
                         ("files", "text_policy_passed_not_execution_approval"))

    def test_binary_with_secret_quarantines_before_magic(self):
        raw = b"\x89PNG\r\n\x1a\n" + b"sk-" + b"a" * 40
        self.assertEqual(MODULE.classify("images/logo.png", raw), ("quarantine", "recognisable_secret_material"))

    def test_invalid_text_quarantines(self):
        for raw in (b"\x80", b"a\x00b"):
            self.assertEqual(MODULE.classify("assets/app.js", raw), ("quarantine", "invalid_text"))

    def test_underscore_frontend_chunks_allowed(self):
        value = envelope(dict(self.files, **{"assets/_dashboard.chunk-A1.js": b"fixture"}))
        MODULE.validate_inventory(value["inventory"])
        self.assertEqual(MODULE.preserve(self.destination, "capture", value)["entry_count"], 4)

    def test_bad_paths_bounds_types_and_extra_fields_rejected(self):
        base = envelope(self.files)["inventory"]
        variants = []
        for path in ("../a.js", "/a.js", "assets/deeper/a.js", "assets/.env.js", "assets/a.sh",
                     "gateway.tar", "web.zip", "images/unreviewed.png", "assets/a;cmd.js", "assets/中文.js"):
            value = copy.deepcopy(base)
            value["entries"][0]["path"] = path
            variants.append(value)
        for field, bad in (("bytes", True), ("bytes", -1), ("bytes", MODULE.MAX_FILE + 1), ("sha256", "x")):
            value = copy.deepcopy(base)
            value["entries"][0][field] = bad
            variants.append(value)
        for field, bad in (("schema_version", True), ("source", {}), ("entries", []), ("unknown", 1)):
            value = copy.deepcopy(base)
            value[field] = bad
            variants.append(value)
        variants.append(dict(base, entries=base["entries"] * 2))
        variants.append(dict(base, entries=base["entries"][::-1]))
        for value in variants:
            with self.subTest(value=value), self.assertRaises(EvidenceError):
                MODULE.validate_inventory(value)

    def test_invalid_payload_rejected_before_creating_destination(self):
        for mutation in (lambda v: v["contents"].update({"extra.js": ""}),
                         lambda v: v["contents"].update({"index.html": "%%%"}),
                         lambda v: v["contents"].update({"index.html": "YWJj"}),
                         lambda v: v["contents"].pop("index.html")):
            value = envelope(self.files)
            mutation(value)
            with self.assertRaises(EvidenceError):
                MODULE.preserve(self.destination, "capture", value)
            self.assertFalse(self.destination.exists())

    def test_existing_git_and_symlink_destinations_rejected(self):
        repo = self.root / "repo"
        repo.mkdir()
        (repo / ".git").mkdir()
        existing = self.root / "existing"
        existing.mkdir()
        link = self.root / "linked"
        link.symlink_to(self.root, target_is_directory=True)
        for target in (repo / "out", existing, link / "out", "relative"):
            with self.subTest(target=target), self.assertRaises(EvidenceError):
                MODULE.preserve(target, "capture", envelope(self.files))

    def test_verify_detects_bytes_permissions_extra_files_and_symlinks(self):
        self.preserve()
        target = self.destination / "files/index.html"
        target.write_bytes(b"changed")
        with self.assertRaises(EvidenceError):
            MODULE.verify(self.destination)
        target.write_bytes(self.files["index.html"])
        target.chmod(0o644)
        with self.assertRaises(EvidenceError):
            MODULE.verify(self.destination)
        target.chmod(0o600)
        extra = self.destination / "extra"
        extra.write_text("fixture")
        extra.chmod(0o600)
        with self.assertRaises(EvidenceError):
            MODULE.verify(self.destination)
        extra.unlink()
        target.unlink()
        target.symlink_to(self.root / "absent")
        with self.assertRaises(EvidenceError):
            MODULE.verify(self.destination)

    def test_receipt_cannot_promote_execution_or_quarantine(self):
        self.preserve()
        path = self.destination / "receipt.json"
        original = json.loads(path.read_text())
        for field, value in (("approved_for_execution", True), ("approved_for_publication", True),
                             ("mode", []), ("records", []), ("unknown", False)):
            doc = copy.deepcopy(original)
            doc[field] = value
            path.write_bytes(json_bytes(doc))
            with self.subTest(field=field), self.assertRaises(EvidenceError):
                MODULE.verify(self.destination)

    def test_ssh_fixed_target_quoting_and_process_bounds(self):
        identity = str(self.root / "key with spaces;'not executed")
        args = MODULE.ssh_command("inventory", identity, "en0", None)
        self.assertIn(identity, args)
        self.assertEqual(args[-2], "root@216.106.185.119")
        self.assertEqual(shlex.split(args[-1])[:4], ["/usr/bin/python3", "-I", "-c", MODULE.REMOTE_SOURCE])
        self.assertIn("StrictHostKeyChecking=yes", args)
        self.assertIn("BindInterface=en0", args)
        self.assertIn("/dev/null", args)
        value = envelope(self.files)
        value["contents"] = {}
        with patch.object(MODULE, "run_bounded", return_value=ProcessResult(0, json_bytes(value))) as runner:
            self.assertEqual(MODULE.request_remote("inventory", identity, "en0", None), value)
            self.assertEqual(runner.call_args.kwargs["timeout_seconds"], 60)
            self.assertEqual(runner.call_args.kwargs["stdout_limit"], 32 * 1024 * 1024)
        with self.assertRaises(EvidenceError):
            MODULE.ssh_command("inventory", identity, "en0;echo", None)

    def test_remote_errors_and_review_mismatch_do_not_echo_output(self):
        value = envelope(self.files)
        for result in (ProcessResult(1, b"synthetic secret"), ProcessResult(0, b'{"inventory":{},"inventory":{}}'),
                       ProcessResult(0, json_bytes(value))):
            with patch.object(MODULE, "run_bounded", return_value=result), self.assertRaises(EvidenceError):
                MODULE.request_remote("capture", self.root / "key", "en0", {})


class RemoteSourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / "public"
        self.root.mkdir()
        (self.root / "assets").mkdir()
        (self.root / "index.html").write_bytes(b"fixture index")
        (self.root / "assets/app.js").write_bytes(b"fixture JS never executed")

    def remote(self, mode="inventory", reviewed=None, script=None):
        source = (script or MODULE.REMOTE_SOURCE).replace('ROOT = "/opt/gateway/public"', "ROOT = " + repr(str(self.root)), 1)
        return run_bounded([sys.executable, "-I", "-c", source, mode, json.dumps(reviewed)],
                           environment={"PATH": "/usr/bin:/bin"}, stdout_limit=MODULE.MAX_OUTPUT, timeout_seconds=10)

    def test_metadata_then_exact_capture_and_never_reads_excluded(self):
        # FIFOs would hang an accidental archive/log read; no such read occurs.
        for name in ("gateway.tar", "web.zip", ".env", "runtime.log"):
            os.mkfifo(self.root / name)
        (self.root / "assets/nested").mkdir()
        (self.root / "assets/nested/hidden.js").write_bytes(b"not recursively collected")
        inventory_result = self.remote()
        self.assertEqual(inventory_result.returncode, 0)
        value = load_json_bytes(inventory_result.stdout)
        self.assertEqual(value["contents"], {})
        self.assertEqual([e["path"] for e in value["inventory"]["entries"]], ["assets/app.js", "index.html"])
        captured = self.remote("capture", value["inventory"])
        self.assertEqual(captured.returncode, 0)
        self.assertEqual(set(load_json_bytes(captured.stdout)["contents"]), {"assets/app.js", "index.html"})

    def test_missing_review_and_changed_files_fail_without_output(self):
        value = load_json_bytes(self.remote().stdout)
        for reviewed in (None, {}):
            result = self.remote("capture", reviewed)
            self.assertEqual((result.returncode, result.stdout), (1, b""))
        (self.root / "assets/app.js").write_bytes(b"changed")
        result = self.remote("capture", value["inventory"])
        self.assertEqual((result.returncode, result.stdout), (1, b""))

    def test_underscore_frontend_chunk_collected(self):
        (self.root / "assets/_dashboard.chunk-A1.js").write_bytes(b"fixture")
        result = self.remote()
        self.assertEqual(result.returncode, 0)
        self.assertIn("assets/_dashboard.chunk-A1.js", [e["path"] for e in load_json_bytes(result.stdout)["inventory"]["entries"]])

    def test_symlink_file_parent_and_special_file_rejected(self):
        path = self.root / "assets/app.js"
        path.unlink()
        path.symlink_to(self.root / "index.html")
        self.assertEqual(self.remote().returncode, 1)
        path.unlink()
        os.mkfifo(path)
        self.assertEqual(self.remote().returncode, 1)
        path.unlink()
        (self.root / "assets").rmdir()
        (self.root / "assets").symlink_to(self.root, target_is_directory=True)
        self.assertEqual(self.remote().returncode, 1)

    def test_unsafe_name_and_file_size_rejected(self):
        unsafe = self.root / "assets/a;command.js"
        unsafe.write_bytes(b"fixture")
        self.assertEqual(self.remote().returncode, 1)
        unsafe.unlink()
        with (self.root / "assets/app.js").open("wb") as handle:
            handle.truncate(MODULE.MAX_FILE + 1)
        self.assertEqual(self.remote().returncode, 1)

    def test_count_and_total_budgets(self):
        script = MODULE.REMOTE_SOURCE.replace("MAX_ENTRIES = 512", "MAX_ENTRIES = 1")
        self.assertEqual(self.remote(script=script).returncode, 1)
        script = MODULE.REMOTE_SOURCE.replace("MAX_TOTAL = 16777216", "MAX_TOTAL = 1")
        self.assertEqual(self.remote(script=script).returncode, 1)

    def test_read_mutation_rejected_before_output(self):
        # Mutate the opened regular fixture immediately after its first read.
        hook = '''original_read = os.read
changed = False
def mutating_read(fd, amount):
    global changed
    raw = original_read(fd, amount)
    if raw and not changed:
        changed = True
        with open(ROOT + "/assets/app.js", "ab") as fixture:
            fixture.write(b"mutation")
    return raw
os.read = mutating_read
'''
        script = MODULE.REMOTE_SOURCE.replace("try:\n    result = collect", hook + "\ntry:\n    result = collect", 1)
        result = self.remote(script=script)
        self.assertEqual((result.returncode, result.stdout), (1, b""))


if __name__ == "__main__":
    unittest.main()
