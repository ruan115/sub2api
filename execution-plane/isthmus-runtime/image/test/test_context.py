"""Offline byte-copy tests; synthetic ar headers are not installable packages."""

import hashlib
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from imagekit import context
from imagekit.lock import ImageError
from imagekit.recipe import render_checksums, render_recipe
from recoverykit.evidence.filesystem import json_bytes


class ContextTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.source = self.root / "source"
        self.source.mkdir()
        self.destination = self.root / "context"
        self.lock_path = self.root / "inputs.json"
        packages = []
        for name in ("ca-certificates", "passwd", "procps", "util-linux"):
            architecture = "all" if name == "ca-certificates" else "amd64"
            filename = name + "_1.0-1_" + architecture + ".deb"
            content = b"!<arch>\n" + name.encode() + b"\nsynthetic, not an installable package\n"
            (self.source / filename).write_bytes(content)
            packages.append({"file": filename, "name": name, "version": "1.0-1",
                             "architecture": architecture, "size": len(content),
                             "sha256": hashlib.sha256(content).hexdigest(),
                             "source_url": "https://deb.debian.org/debian/pool/main/p/fixture/" + filename})
        self.lock = {"schema_version": 1, "kind": "isthmus-base-build-inputs",
                     "platform": "linux/amd64", "base_image": "docker.io/library/debian@sha256:" + "a" * 64,
                     "packages": packages}
        self.write_lock()

    def write_lock(self):
        self.lock_path.write_bytes(json_bytes(self.lock))

    def stage(self, destination=None):
        return context.stage_context(self.lock_path, self.source, destination or self.destination)

    def test_roundtrip_exact_inventory_private_modes_and_no_execution_permission(self):
        source_before = {p["file"]: (self.source / p["file"]).read_bytes() for p in self.lock["packages"]}
        report = self.stage()
        self.assertEqual(report, {"status": "base_context_verified", "image_built": False,
                                  "execution_permitted": False, "artifact_count": 4,
                                  "total_bytes": sum(p["size"] for p in self.lock["packages"])})
        self.assertEqual(report, context.verify_context(self.destination))
        self.assertEqual({p.name for p in self.destination.iterdir()},
                         {"packages", "Dockerfile", "packages.sha256", "artifacts.lock.json", "receipt.json"})
        self.assertEqual((self.destination / "Dockerfile").read_bytes(), render_recipe(self.lock))
        self.assertEqual((self.destination / "packages.sha256").read_bytes(), render_checksums(self.lock))
        for root, directories, files in os.walk(self.destination):
            self.assertEqual(stat.S_IMODE(os.stat(root).st_mode), 0o700)
            for name in files:
                self.assertEqual(stat.S_IMODE(os.stat(Path(root) / name).st_mode), 0o600)
        for name, content in source_before.items():
            self.assertEqual((self.source / name).read_bytes(), content)
            self.assertEqual((self.destination / "packages" / name).read_bytes(), content)

    def test_all_paths_must_be_explicit_absolute(self):
        for args in ((Path("inputs.json"), self.source, self.destination),
                     (self.lock_path, Path("source"), self.destination),
                     (self.lock_path, self.source, Path("context"))):
            with self.subTest(argument=str(args)), self.assertRaises(ImageError):
                context.stage_context(*args)
        with self.assertRaises(ImageError):
            context.verify_context(Path("context"))
        self.assertFalse(self.destination.exists())

    def test_existing_source_nested_and_git_destinations_are_rejected_without_overwrite(self):
        existing = self.root / "existing"
        existing.mkdir()
        marker = existing / "untouched"
        marker.write_bytes(b"keep")
        gitroot = self.root / "repository"
        gitroot.mkdir()
        (gitroot / ".git").write_bytes(b"gitdir: synthetic")
        for destination in (existing, self.source / "nested", gitroot / "output"):
            with self.subTest(name=destination.name), self.assertRaises(ImageError):
                self.stage(destination)
        self.assertEqual(marker.read_bytes(), b"keep")
        self.assertFalse((gitroot / "output").exists())

    def test_source_path_symlink_ancestors_and_destination_symlink_are_rejected(self):
        alias = self.root / "alias"
        alias.symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(ImageError):
            context.stage_context(self.lock_path, alias, self.destination)
        self.destination.symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(ImageError):
            self.stage()

    def test_missing_corrupt_and_wrong_size_packages_fail_before_destination_creation(self):
        target = self.source / self.lock["packages"][0]["file"]
        original = target.read_bytes()
        for case in ("missing", "same-size-corrupt", "short", "long"):
            with self.subTest(case=case):
                if target.exists():
                    target.unlink()
                if case != "missing":
                    data = {"same-size-corrupt": b"x" * len(original), "short": original[:-1], "long": original + b"x"}[case]
                    target.write_bytes(data)
                with self.assertRaises(ImageError):
                    self.stage()
                self.assertFalse(self.destination.exists())
        target.write_bytes(original)

    def test_source_symlink_fifo_and_hardlink_are_rejected_before_copy(self):
        target = self.source / self.lock["packages"][0]["file"]
        original = target.read_bytes()
        outside = self.root / "outside.deb"
        outside.write_bytes(original)
        for kind in ("symlink", "fifo", "hardlink"):
            with self.subTest(kind=kind):
                target.unlink()
                if kind == "symlink":
                    target.symlink_to(outside)
                elif kind == "fifo":
                    os.mkfifo(target)
                else:
                    os.link(outside, target)
                with self.assertRaises(ImageError):
                    self.stage()
                self.assertFalse(self.destination.exists())

    def test_lock_symlink_and_hardlink_are_rejected(self):
        original = self.root / "original-inputs.json"
        self.lock_path.rename(original)
        for kind in ("symlink", "hardlink"):
            if os.path.lexists(self.lock_path):
                self.lock_path.unlink()
            if kind == "symlink":
                self.lock_path.symlink_to(original)
            else:
                os.link(original, self.lock_path)
            with self.subTest(kind=kind), self.assertRaises(ImageError):
                self.stage()
        self.assertFalse(self.destination.exists())

    def test_replay_preserves_existing_complete_context(self):
        self.stage()
        before = (self.destination / "receipt.json").read_bytes()
        with self.assertRaises(ImageError):
            self.stage()
        self.assertEqual(before, (self.destination / "receipt.json").read_bytes())
        context.verify_context(self.destination)

    def test_source_changed_during_read_is_rejected(self):
        target = self.source / self.lock["packages"][0]["file"]
        inode = target.stat().st_ino
        original_read = os.read
        changed = False

        def read_and_change(fd, size):
            nonlocal changed
            data = original_read(fd, size)
            if not changed and os.fstat(fd).st_ino == inode:
                changed = True
                target.write_bytes(b"changed")
            return data

        with patch.object(context.os, "read", side_effect=read_and_change), self.assertRaises(ImageError):
            self.stage()
        self.assertTrue(changed)
        self.assertFalse(self.destination.exists())

    def test_valid_digest_does_not_replace_minimal_ar_header_check(self):
        package = self.lock["packages"][0]
        content = b"not-ar!!synthetic bytes"
        (self.source / package["file"]).write_bytes(content)
        package.update(size=len(content), sha256=hashlib.sha256(content).hexdigest())
        self.write_lock()
        with self.assertRaises(ImageError):
            self.stage()
        self.assertFalse(self.destination.exists())

    def test_small_chunks_and_partial_writes_are_fully_copied(self):
        original_write = os.write

        def partial_write(fd, data):
            return original_write(fd, data[:3])

        with patch.object(context, "_CHUNK", 7), patch.object(context.os, "write", side_effect=partial_write):
            report = self.stage()
        self.assertEqual(report, context.verify_context(self.destination))

    def test_zero_byte_copy_write_fails_without_completion_receipt(self):
        with patch.object(context.os, "write", return_value=0), self.assertRaises(ImageError):
            self.stage()
        self.assertFalse((self.destination / "receipt.json").exists())

    def test_source_replaced_with_same_bytes_between_preflight_and_copy_is_rejected(self):
        target = self.source / self.lock["packages"][0]["file"]
        original_create = context.create_private_destination

        def create_then_replace(*args, **kwargs):
            original_create(*args, **kwargs)
            content = target.read_bytes()
            target.rename(self.root / "old.deb")
            target.write_bytes(content)

        with patch.object(context, "create_private_destination", side_effect=create_then_replace), self.assertRaises(ImageError):
            self.stage()
        self.assertFalse((self.destination / "receipt.json").exists())

    def test_source_replaced_after_copy_is_rejected_before_receipt(self):
        target = self.source / self.lock["packages"][0]["file"]
        original_write = context.write_exclusive

        def write_then_replace(root, name, content):
            original_write(root, name, content)
            if name == "Dockerfile":
                previous = target.read_bytes()
                target.rename(self.root / "old.deb")
                target.write_bytes(previous)

        with patch.object(context, "write_exclusive", side_effect=write_then_replace), self.assertRaises(ImageError):
            self.stage()
        self.assertFalse((self.destination / "receipt.json").exists())

    def test_write_failure_never_emits_completion_receipt(self):
        original_write = context.write_exclusive

        def fail_dockerfile(root, name, data):
            if name == "Dockerfile":
                raise OSError("synthetic-private-error")
            original_write(root, name, data)

        with patch.object(context, "write_exclusive", side_effect=fail_dockerfile), self.assertRaises(ImageError) as caught:
            self.stage()
        self.assertNotIn("synthetic-private-error", str(caught.exception))
        self.assertFalse((self.destination / "receipt.json").exists())
        with self.assertRaises(ImageError):
            context.verify_context(self.destination)

    def test_final_verification_failure_returns_error_even_if_receipt_exists(self):
        original_verify = context._verify

        def fail_final_verify(destination, *, complete):
            if complete:
                raise OSError("synthetic-private-error")
            return original_verify(destination, complete=complete)

        with patch.object(context, "_verify", side_effect=fail_final_verify), self.assertRaises(ImageError) as caught:
            self.stage()
        self.assertNotIn("synthetic-private-error", str(caught.exception))
        self.assertTrue((self.destination / "receipt.json").exists())
        # No automatic deletion: only a later successful full verification can
        # establish that this private output is usable as a build context.
        self.assertEqual(context.verify_context(self.destination)["status"], "base_context_verified")

    def test_context_file_tampering_extra_files_and_empty_directories_are_rejected(self):
        for name in ("Dockerfile", "packages.sha256", "artifacts.lock.json", "receipt.json", "packages/" + self.lock["packages"][0]["file"], "extra", "directory"):
            with self.subTest(name=name):
                destination = self.root / ("case-" + str(len(list(self.root.iterdir()))))
                self.stage(destination)
                if name == "directory":
                    (destination / "extra-directory").mkdir(mode=0o700)
                elif name == "extra":
                    (destination / "packages/extra.deb").write_bytes(b"extra")
                    (destination / "packages/extra.deb").chmod(0o600)
                else:
                    (destination / name).write_bytes(b"tampered")
                with self.assertRaises(ImageError):
                    context.verify_context(destination)

    def test_context_permissions_symlinks_and_hardlinks_are_rejected(self):
        for case in ("root-mode", "packages-mode", "file-mode", "symlink", "hardlink"):
            with self.subTest(case=case):
                destination = self.root / case
                self.stage(destination)
                target = destination / "packages" / self.lock["packages"][0]["file"]
                if case == "root-mode":
                    destination.chmod(0o755)
                elif case == "packages-mode":
                    (destination / "packages").chmod(0o755)
                elif case == "file-mode":
                    target.chmod(0o644)
                elif case == "symlink":
                    target.unlink()
                    target.symlink_to(self.source / target.name)
                else:
                    os.link(target, self.root / "additional-link.deb")
                with self.assertRaises(ImageError):
                    context.verify_context(destination)

    def test_receipt_unknown_fields_and_bool_counter_are_rejected(self):
        for key, value in (("extra", "synthetic"), ("schema_version", True), ("artifact_count", True), ("total_bytes", False)):
            destination = self.root / ("receipt-" + key)
            self.stage(destination)
            receipt = json.loads((destination / "receipt.json").read_bytes())
            receipt[key] = value
            (destination / "receipt.json").write_bytes(json_bytes(receipt))
            with self.assertRaises(ImageError):
                context.verify_context(destination)

    def test_verification_needs_no_source_lock_or_source_packages(self):
        self.stage()
        self.lock_path.unlink()
        for path in self.source.iterdir():
            path.unlink()
        self.source.rmdir()
        self.assertEqual(context.verify_context(self.destination)["artifact_count"], 4)

    def test_lock_changed_after_parse_before_inventory_is_rejected(self):
        self.stage()
        original_validate = context.validate_lock

        def validate_then_change(document):
            validated = original_validate(document)
            (self.destination / "artifacts.lock.json").write_bytes(b"invalid replacement")
            return validated

        with patch.object(context, "validate_lock", side_effect=validate_then_change), self.assertRaises(ImageError):
            context.verify_context(self.destination)

    def test_self_consistent_different_context_cannot_replace_requested_lock(self):
        original_write = context.write_exclusive

        def write_then_replace(root, name, content):
            original_write(root, name, content)
            if name == "artifacts.lock.json":
                replacement = json.loads(content)
                replacement["base_image"] = "docker.io/library/debian@sha256:" + "b" * 64
                (root / name).write_bytes(json_bytes(replacement))
                (root / "Dockerfile").write_bytes(render_recipe(replacement))

        with patch.object(context, "write_exclusive", side_effect=write_then_replace), self.assertRaises(ImageError):
            self.stage()
        self.assertFalse((self.destination / "receipt.json").exists())


if __name__ == "__main__":
    unittest.main()
