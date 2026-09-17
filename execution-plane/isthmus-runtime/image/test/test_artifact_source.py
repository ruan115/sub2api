"""Offline Git plumbing and private source-artifact tests; no app execution."""

import copy
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import time
import unittest
from unittest.mock import patch

from artifacts import source
from recoverykit.evidence.filesystem import json_bytes
from recoverykit.workspace.process import ProcessError, ProcessResult


IMAGE_ROOT = Path(__file__).resolve().parents[1]
REPOSITORY = IMAGE_ROOT.parents[2]
LOCK_PATH = IMAGE_ROOT / "locks/app-fake-source-2026-09-17.json"


class SourceTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.dest = self.root / "artifact"
        self.lock_path = self.root / "input.json"
        self.lock = json.loads(LOCK_PATH.read_bytes())
        self.lock_path.write_bytes(json_bytes(self.lock))

    def stage(self, repo=REPOSITORY, dest=None):
        return source.stage_source(repo, self.lock_path, dest or self.dest)

    def test_roundtrip_precise_whitelist_hashes_modes_and_capability(self):
        report = self.stage()
        self.assertEqual(report, {"status": "fake_source_verified", "capability": "fake-only",
                                 "execution_permitted": False, "source_commit": source.SOURCE_COMMIT,
                                 "file_count": 23, "total_bytes": 47550})
        self.assertEqual(source.verify_source(self.dest), report)
        actual = {str(p.relative_to(self.dest / "source"))
                  for p in (self.dest / "source").rglob("*") if p.is_file()}
        self.assertEqual(actual, set(source.SOURCE_PATHS))
        self.assertEqual(len(actual), 23)
        self.assertNotIn("provenance.json", " ".join(actual))
        for directory, _, files in os.walk(self.dest):
            self.assertEqual(stat.S_IMODE(os.stat(directory).st_mode), 0o700)
            for name in files:
                self.assertEqual(stat.S_IMODE(os.stat(Path(directory) / name).st_mode), 0o600)
        receipt = json.loads((self.dest / "receipt.json").read_bytes())
        self.assertEqual(receipt["lock_sha256"], hashlib.sha256(json_bytes(self.lock)).hexdigest())
        self.assertNotIn(str(self.root), json.dumps(receipt))
        self.assertNotIn(str(REPOSITORY), json.dumps(receipt))

    def test_commit_source_ignores_modified_missing_symlink_and_extra_worktree_files(self):
        # Local-only shared clone: no checkout, network, commits or source execution.
        clone = self.root / "repository"
        result = subprocess.run(["/usr/bin/git", "clone", "--local", "--shared", "--no-checkout",
                                 str(REPOSITORY), str(clone)], stdin=subprocess.DEVNULL,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                env={"PATH": "/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM": "1",
                                     "GIT_CONFIG_GLOBAL": "/dev/null"}, timeout=20)
        self.assertEqual(result.returncode, 0)
        worktree = clone / source.SOURCE_ROOT
        (worktree / "src/app").mkdir(parents=True)
        (worktree / "package.json").write_text("modified, not the selected commit")
        (worktree / "src/app/fake.ts").symlink_to(self.lock_path)
        (worktree / "extra.env").write_text("synthetic-not-for-release")
        (worktree / "src/app/injected.test.ts").write_text("throw new Error('not executed')")
        self.stage(repo=clone)
        for record in self.lock["files"]:
            actual = (self.dest / "source" / record["path"]).read_bytes()
            self.assertEqual(hashlib.sha256(actual).hexdigest(), record["sha256"])
        self.assertFalse((self.dest / "source/extra.env").exists())
        self.assertEqual((worktree / "package.json").read_text(), "modified, not the selected commit")
        self.assertTrue((worktree / "src/app/fake.ts").is_symlink())

    def test_lock_schema_exact_fields_counts_limits_and_fixed_source(self):
        bad = []
        for field, value in (("schema_version", True), ("kind", "real-runtime"),
                             ("capability", "production"), ("source_commit", "0" * 40),
                             ("source_root", "../other"), ("file_count", True),
                             ("file_count", 24), ("total_bytes", True),
                             ("total_bytes", 0), ("total_bytes", source.MAX_TOTAL_BYTES + 1),
                             ("total_bytes", 47551), ("files", {})):
            candidate = copy.deepcopy(self.lock)
            candidate[field] = value
            bad.append(candidate)
        for field in self.lock:
            candidate = copy.deepcopy(self.lock)
            del candidate[field]
            bad.append(candidate)
        candidate = copy.deepcopy(self.lock)
        candidate["provenance"] = "/private/synthetic"
        bad.append(candidate)
        for mutation in (lambda files: files.pop(), lambda files: files.append(files[0]),
                         lambda files: files.reverse()):
            candidate = copy.deepcopy(self.lock)
            mutation(candidate["files"])
            bad.append(candidate)
        for field, value in (("path", "../outside"), ("path", "src/app/fake.test.ts"),
                             ("mode", "120000"), ("mode", "100755"),
                             ("size_bytes", True), ("size_bytes", -1),
                             ("size_bytes", source.MAX_FILE_BYTES + 1),
                             ("git_blob_oid", "A" * 40), ("git_blob_oid", None),
                             ("sha256", "a" * 63), ("sha256", None)):
            candidate = copy.deepcopy(self.lock)
            candidate["files"][0][field] = value
            bad.append(candidate)
        candidate = copy.deepcopy(self.lock)
        candidate["files"][0]["extra"] = True
        bad.append(candidate)
        for index, candidate in enumerate(bad):
            with self.subTest(index=index), self.assertRaises(source.SourceError):
                source.validate_lock(candidate)
        self.assertEqual(source.validate_lock(self.lock), self.lock)

    def test_invalid_lock_json_and_duplicate_fields_rejected(self):
        for data in (b'{"schema_version":1,"schema_version":1}', b"\xff", b"{}", b"[", b"null"):
            self.lock_path.write_bytes(data)
            with self.subTest(data=data), self.assertRaises(source.SourceError):
                self.stage()
        self.assertFalse(self.dest.exists())

    def test_git_tree_rejects_missing_extra_symlink_executable_and_wrong_oid_before_output(self):
        real = source._git
        for mutation in (lambda raw: raw.split(b"\0", 1)[1],
                         lambda raw: raw + raw.split(b"\0", 1)[0] + b"\0",
                         lambda raw: raw.replace(b"100644", b"120000", 1),
                         lambda raw: raw.replace(b"100644", b"100755", 1),
                         lambda raw: raw.replace(self.lock["files"][0]["git_blob_oid"].encode(), b"0" * 40, 1)):
            def fake(repo, args, **kwargs):
                data = real(repo, args, **kwargs)
                return mutation(data) if args[0] == "ls-tree" else data
            with self.subTest(mutation=mutation), patch.object(source, "_git", side_effect=fake):
                with self.assertRaises(source.SourceError):
                    self.stage()
            self.assertFalse(self.dest.exists())

    def test_blob_hash_and_size_mismatch_rejected_before_output(self):
        real = source._git
        for mutation in (lambda raw: raw + b"x", lambda raw: bytes([raw[0] ^ 1]) + raw[1:]):
            def fake(repo, args, **kwargs):
                data = real(repo, args, **kwargs)
                return mutation(data) if args[0] == "cat-file" else data
            with patch.object(source, "_git", side_effect=fake), self.assertRaises(source.SourceError):
                self.stage()
            self.assertFalse(self.dest.exists())

    def test_repository_and_all_paths_must_be_explicit_nonsymlink_root(self):
        alias = self.root / "alias"
        alias.symlink_to(REPOSITORY, target_is_directory=True)
        for args in ((Path("repo"), self.lock_path, self.dest),
                     (REPOSITORY, Path("input.json"), self.dest),
                     (REPOSITORY, self.lock_path, Path("artifact")),
                     (alias, self.lock_path, self.dest),
                     (REPOSITORY / "execution-plane", self.lock_path, self.dest)):
            with self.subTest(args=args), self.assertRaises(source.SourceError):
                source.stage_source(*args)
        with self.assertRaises(source.SourceError):
            source.verify_source(Path("artifact"))
        self.assertFalse(self.dest.exists())

    def test_lock_links_and_existing_destination_rejected(self):
        for link in ("symlink", "hardlink"):
            path = self.root / link
            if link == "symlink":
                path.symlink_to(self.lock_path)
            else:
                os.link(self.lock_path, path)
            with self.assertRaises(source.SourceError):
                source.stage_source(REPOSITORY, path, self.dest)
            path.unlink()
        self.dest.mkdir()
        marker = self.dest / "untouched"
        marker.write_text("synthetic")
        with self.assertRaises(source.SourceError):
            self.stage()
        self.assertEqual(marker.read_text(), "synthetic")

    def test_exact_inventory_rejects_extra_empty_directory_missing_tamper_links_and_mode(self):
        mutations = (
            lambda root: (root / "source/extra.env").write_text("synthetic"),
            lambda root: (root / "source/empty").mkdir(mode=0o700),
            lambda root: (root / "source/package.json").unlink(),
            lambda root: (root / "source/package.json").write_bytes(b"changed"),
            lambda root: (root / "source/package.json").chmod(0o644),
            lambda root: (root / "source/src").chmod(0o755),
            lambda root: (root / "source/package.json").rename(root / "source/package.old"),
        )
        for index, mutation in enumerate(mutations):
            destination = self.root / ("case-" + str(index))
            self.stage(dest=destination)
            mutation(destination)
            with self.subTest(index=index), self.assertRaises(source.SourceError):
                source.verify_source(destination)
        for kind in ("symlink", "hardlink"):
            destination = self.root / kind
            self.stage(dest=destination)
            target = destination / "source/package.json"
            original = self.root / (kind + "-original")
            target.rename(original)
            if kind == "symlink":
                target.symlink_to(original)
            else:
                os.link(original, target)
            with self.assertRaises(source.SourceError):
                source.verify_source(destination)

    def test_receipt_canonical_and_strict_false_not_integer_zero(self):
        self.stage()
        receipt = self.dest / "receipt.json"
        value = json.loads(receipt.read_bytes())
        value["execution_permitted"] = 0
        receipt.write_bytes(json_bytes(value))
        with self.assertRaises(source.SourceError):
            source.verify_source(self.dest)

    def test_lock_same_bytes_replacement_during_git_read_rejected(self):
        real = source._collect
        def replace(repo, lock):
            result = real(repo, lock)
            replacement = self.root / "replacement.json"
            replacement.write_bytes(self.lock_path.read_bytes())
            replacement.replace(self.lock_path)
            return result
        with patch.object(source, "_collect", side_effect=replace), self.assertRaises(source.SourceError):
            self.stage()
        self.assertFalse(self.dest.exists())

    def test_final_verify_failure_does_not_return_success_even_if_receipt_exists(self):
        real = source._verify
        def fail(destination, *, complete):
            if complete:
                raise OSError("synthetic-private-path-must-not-escape")
            return real(destination, complete=complete)
        with patch.object(source, "_verify", side_effect=fail):
            with self.assertRaisesRegex(source.SourceError, "^source_stage_failed$"):
                self.stage()
        self.assertTrue((self.dest / "receipt.json").exists())

    def test_git_process_limits_clean_environment_and_fixed_error_categories(self):
        with patch.dict(os.environ, {"GIT_SSH_COMMAND": "synthetic", "HTTPS_PROXY": "synthetic",
                                     "SYNTHETIC_SECRET": "never-pass"}):
            with patch("recoverykit.workspace.process.run_bounded", return_value=ProcessResult(0, b"ok")) as run:
                self.assertEqual(source._git(REPOSITORY, ["rev-parse", "--show-toplevel"],
                                             limit=1024, deadline=time.monotonic() + 30), b"ok")
                kwargs = run.call_args.kwargs
                self.assertEqual(kwargs["stdout_limit"], 1024)
                self.assertEqual(kwargs["stderr_limit"], 16384)
                self.assertGreater(kwargs["timeout_seconds"], 0)
                self.assertLessEqual(kwargs["timeout_seconds"], 30)
                self.assertEqual(kwargs["environment"], source._ENVIRONMENT)
                self.assertNotIn("HTTPS_PROXY", kwargs["environment"])
                self.assertEqual(kwargs["environment"]["GIT_ALLOW_PROTOCOL"], "")
                command = run.call_args.args[0]
                self.assertIn("--no-replace-objects", command)
                self.assertIn("protocol.allow=never", command)
        for result in (ProcessResult(1, b"synthetic-private-data"), ProcessError("private-detail")):
            with patch("recoverykit.workspace.process.run_bounded", side_effect=result if isinstance(result, Exception) else None,
                              return_value=result):
                with self.assertRaisesRegex(source.SourceError, "^source_git_failed$"):
                    source._git(REPOSITORY, [], limit=1, deadline=time.monotonic() + 1)
        with patch("recoverykit.workspace.process.run_bounded") as run:
            with self.assertRaises(source.SourceError):
                source._git(REPOSITORY, [], limit=1, deadline=time.monotonic() - 1)
            run.assert_not_called()

    def test_output_same_byte_replacement_during_verification_is_rejected(self):
        self.stage()
        real = source._check_bytes
        changed = False
        def replace(record, data):
            nonlocal changed
            real(record, data)
            if not changed:
                changed = True
                path = self.dest / "source" / record["path"]
                replacement = self.root / "same-bytes"
                replacement.write_bytes(data)
                replacement.chmod(0o600)
                replacement.replace(path)
        with patch.object(source, "_check_bytes", side_effect=replace), self.assertRaises(source.SourceError):
            source.verify_source(self.dest)

    def test_input_replacement_after_complete_verification_prevents_stage_success(self):
        real = source._verify
        def replace(destination, *, complete):
            receipt = real(destination, complete=complete)
            if complete:
                replacement = self.root / "replacement.json"
                replacement.write_bytes(self.lock_path.read_bytes())
                replacement.replace(self.lock_path)
            return receipt
        with patch.object(source, "_verify", side_effect=replace), self.assertRaisesRegex(
                source.SourceError, "^source_lock_changed$"):
            self.stage()

    def test_zero_progress_write_fails_closed_without_receipt(self):
        # Git's subprocess pipe reads do not use os.write in this process.
        with patch.object(source.os, "write", return_value=0):
            with self.assertRaisesRegex(source.SourceError, "^source_write_failed$"):
                self.stage()
        self.assertFalse((self.dest / "receipt.json").exists())


if __name__ == "__main__":
    unittest.main()
