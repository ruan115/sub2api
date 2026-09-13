from __future__ import annotations

import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from recoverykit.workspace import WorkspaceError, snapshot_workspace, verify_snapshot
from recoverykit.workspace import snapshot as implementation


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        if not shutil.which("git"):
            self.skipTest("Git required")
        self.temp = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.destination = self.root / "snapshot"
        self.git("init", "--quiet")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "user.name", "Offline Fixture")
        (self.repo / "text.txt").write_text("base\n")
        (self.repo / "binary.bin").write_bytes(bytes(range(256)))
        (self.repo / ".gitignore").write_text("ignored/\n")
        self.git("add", ".")
        self.git("commit", "--quiet", "-m", "fixture base")

    def git(self, *args, cwd=None):
        environment = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        environment.update(GIT_OPTIONAL_LOCKS="0", GIT_TERMINAL_PROMPT="0")
        result = subprocess.run(["git", "-C", str(cwd or self.repo), *args], env=environment,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=True)
        return result.stdout

    def dirty(self):
        (self.repo / "text.txt").write_text("staged\n")
        self.git("add", "text.txt")
        (self.repo / "text.txt").write_text("staged plus unstaged\n")
        (self.repo / "binary.bin").write_bytes(bytes(range(255, -1, -1)) * 3)
        (self.repo / "new folder").mkdir()
        untracked = self.repo / "new folder/script.sh"
        untracked.write_text("#!/bin/sh\nexit 0\n")
        untracked.chmod(0o755)
        (self.repo / "ignored").mkdir()
        (self.repo / "ignored/private.txt").write_text("ignored fixture")

    def test_dirty_repo_roundtrip_and_index_unchanged(self):
        self.dirty()
        index = self.repo / ".git/index"
        before_index = index.read_bytes()
        before_status = self.git("status", "--porcelain=v1", "-z", "--untracked-files=all")
        before_head = self.git("rev-parse", "HEAD")
        report = snapshot_workspace(self.repo, self.destination)
        self.assertEqual(report, verify_snapshot(self.destination))
        self.assertEqual(report["untracked_count"], 1)
        self.assertEqual(before_index, index.read_bytes())
        self.assertEqual(before_status, self.git("status", "--porcelain=v1", "-z", "--untracked-files=all"))
        self.assertEqual(before_head, self.git("rev-parse", "HEAD"))
        self.assertEqual((self.destination / "untracked/new folder/script.sh").read_bytes(),
                         (self.repo / "new folder/script.sh").read_bytes())
        self.assertFalse((self.destination / "untracked/ignored").exists())
        self.assertIn(b"GIT binary patch", (self.destination / "git/tracked.patch").read_bytes())
        metadata = json.loads((self.destination / "manifest.json").read_text())
        script = next(r for r in metadata["files"] if r["path"].endswith("script.sh"))
        self.assertEqual(script["source_mode"], 0o755)
        for directory, _, names in os.walk(self.destination):
            self.assertEqual(stat.S_IMODE(os.stat(directory).st_mode), 0o700)
            for name in names:
                self.assertEqual(stat.S_IMODE(os.stat(os.path.join(directory, name)).st_mode), 0o600)
        json.dumps(report)

    def test_binary_staged_and_unstaged_patches_reconstruct_fixture(self):
        self.dirty()
        snapshot_workspace(self.repo, self.destination)
        reconstructed = self.root / "reconstructed"
        self.git("clone", "--quiet", "--no-hardlinks", str(self.repo), str(reconstructed))
        self.git("apply", "--binary", "--index", str(self.destination / "git/staged.patch"), cwd=reconstructed)
        self.git("apply", "--binary", str(self.destination / "git/unstaged.patch"), cwd=reconstructed)
        for name in ["text.txt", "binary.bin"]:
            self.assertEqual((self.repo / name).read_bytes(), (reconstructed / name).read_bytes())
        self.assertEqual(self.git("diff", "--cached", "--binary"),
                         self.git("diff", "--cached", "--binary", cwd=reconstructed))

    def test_detached_head_clean_snapshot(self):
        self.git("checkout", "--detach", "--quiet")
        report = snapshot_workspace(self.repo, self.destination)
        self.assertIsNone(report["branch"])
        self.assertEqual(report["untracked_count"], 0)
        self.assertEqual((self.destination / "git/tracked.patch").read_bytes(), b"")

    def test_deleted_and_renamed_tracked_files_preserved(self):
        self.git("mv", "text.txt", "renamed.txt")
        (self.repo / "binary.bin").unlink()
        snapshot_workspace(self.repo, self.destination)
        combined = (self.destination / "git/tracked.patch").read_bytes()
        self.assertIn(b"deleted file mode", combined)
        self.assertIn(b"new file mode", combined)
        self.assertEqual(verify_snapshot(self.destination)["status"], "verified")

    def test_relative_existing_nested_symlink_and_other_git_destinations_fail(self):
        existing = self.root / "existing"
        existing.mkdir()
        symlink = self.root / "linked"
        symlink.symlink_to(self.root, target_is_directory=True)
        other = self.root / "other"
        other.mkdir()
        (other / ".git").mkdir()
        for target in ["relative", existing, self.repo / "inside", symlink / "out", other / "out"]:
            with self.subTest(target=str(target)):
                with self.assertRaises(WorkspaceError):
                    snapshot_workspace(self.repo, target)

    def test_replay_and_tampering_fail(self):
        self.dirty()
        snapshot_workspace(self.repo, self.destination)
        with self.assertRaises(WorkspaceError):
            snapshot_workspace(self.repo, self.destination)
        (self.destination / "untracked/new folder/script.sh").write_bytes(b"tampered")
        with self.assertRaises(WorkspaceError):
            verify_snapshot(self.destination)

    def test_sensitive_file_refuses_entire_snapshot(self):
        (self.repo / ".env").write_text("synthetic fixture")
        with self.assertRaises(WorkspaceError):
            snapshot_workspace(self.repo, self.destination)
        self.assertFalse(self.destination.exists())

    def test_metadata_limit_checked_before_creating_destination(self):
        with patch.object(implementation, "MAX_METADATA_BYTES", 1):
            with self.assertRaisesRegex(WorkspaceError, "canonical workspace metadata exceeds size limit"):
                snapshot_workspace(self.repo, self.destination)
        self.assertFalse(self.destination.exists())

    def test_git_fsmonitor_hook_is_disabled(self):
        marker = self.root / "fsmonitor-ran"
        hook = self.root / "fsmonitor.sh"
        hook.write_text("#!/bin/sh\ntouch '" + str(marker) + "'\n")
        hook.chmod(0o755)
        self.git("config", "core.fsmonitor", str(hook))
        snapshot_workspace(self.repo, self.destination)
        self.assertFalse(marker.exists())

    def test_git_clean_and_process_filters_fail_before_execution(self):
        for kind in ["clean", "process"]:
            with self.subTest(kind=kind):
                marker = self.root / ("filter-" + kind + "-ran")
                self.git("config", "filter.danger." + kind, "touch '" + str(marker) + "'")
                (self.repo / ".gitattributes").write_text("text.txt filter=danger\n")
                (self.repo / "text.txt").write_text("changed\n")
                with self.assertRaises(WorkspaceError):
                    snapshot_workspace(self.repo, self.destination)
                self.assertFalse(marker.exists())
                self.assertFalse(self.destination.exists())
                self.git("config", "--unset", "filter.danger." + kind)

    def test_untracked_and_changed_tracked_symlinks_fail(self):
        target = self.root / "outside.txt"
        target.write_text("outside")
        link = self.repo / "link.txt"
        link.symlink_to(target)
        with self.assertRaises(WorkspaceError):
            snapshot_workspace(self.repo, self.destination)
        link.unlink()
        tracked = self.repo / "text.txt"
        tracked.unlink()
        tracked.symlink_to(target)
        with self.assertRaises(WorkspaceError):
            snapshot_workspace(self.repo, self.destination)

    def test_metadata_and_extra_files_fail_integrity(self):
        snapshot_workspace(self.repo, self.destination)
        metadata = json.loads((self.destination / "manifest.json").read_text())
        metadata["head"] = "0" * 40
        (self.destination / "manifest.json").write_text(json.dumps(metadata))
        with self.assertRaises(WorkspaceError):
            verify_snapshot(self.destination)
        clean = self.root / "second"
        snapshot_workspace(self.repo, clean)
        extra = clean / "extra.txt"
        extra.write_text("extra")
        extra.chmod(0o600)
        with self.assertRaises(WorkspaceError):
            verify_snapshot(clean)

    def test_post_capture_edit_leaves_no_completion_manifest(self):
        self.dirty()
        original = implementation.write_exclusive
        edited = False

        def edit(root, path, content):
            nonlocal edited
            original(root, path, content)
            if not edited:
                (self.repo / "text.txt").write_text("concurrent edit\n")
                edited = True

        with patch.object(implementation, "write_exclusive", side_effect=edit):
            with self.assertRaises(WorkspaceError):
                snapshot_workspace(self.repo, self.destination)
        self.assertTrue(self.destination.is_dir())
        self.assertFalse((self.destination / "manifest.json").exists())
        with self.assertRaises(WorkspaceError):
            verify_snapshot(self.destination)

    def test_verification_does_not_need_original_repository(self):
        self.dirty()
        snapshot_workspace(self.repo, self.destination)
        self.repo.rename(self.root / "moved-repo")
        self.assertEqual(verify_snapshot(self.destination)["status"], "verified")
