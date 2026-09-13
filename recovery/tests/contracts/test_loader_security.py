"""Security regressions for descriptor-bound contract catalog loading."""

from __future__ import annotations

import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from recoverykit.contracts import ContractError, load_catalog
from recoverykit.contracts import filesystem as catalog_filesystem
from recoverykit.contracts import loader as catalog_loader


class ContractLoaderSecurityTests(unittest.TestCase):
    def setUp(self):
        # macOS exposes its normal temporary directory under `/var`, a
        # compatibility symlink.  Production inputs must reject that spelling;
        # fixtures instead create a physical path deliberately.
        self._temporary_directory = tempfile.TemporaryDirectory(
            dir=str(Path(tempfile.gettempdir()).resolve())
        )
        self.base = Path(self._temporary_directory.name)

    def tearDown(self):
        self._temporary_directory.cleanup()

    def _catalog(self, name, modules=("identity",), raw=b"{}"):
        root = self.base / name / "catalog"
        for module in modules:
            self._write_manifest(root, "portunex", module, raw)
        return root

    @staticmethod
    def _write_manifest(root, owner, module, raw):
        path = root / owner / module / "manifest.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
        return path

    def _assert_filesystem_error(self, root, forbidden_text=None):
        with self.assertRaises(ContractError) as raised:
            load_catalog(root)
        self.assertEqual(("catalog filesystem safety check failed",), raised.exception.issues)
        if forbidden_text is not None:
            self.assertNotIn(forbidden_text, str(raised.exception))

    def _assert_limit_error(self, root):
        with self.assertRaises(ContractError) as raised:
            load_catalog(root)
        self.assertEqual(("catalog input exceeds safety limits",), raised.exception.issues)

    def test_physical_and_simple_relative_roots_preserve_loader_interface(self):
        root = self._catalog("physical-root")
        catalog = load_catalog(root)
        self.assertEqual(1, len(catalog["manifests"]))
        self.assertEqual("portunex/identity/manifest.json", catalog["manifests"][0]["path"])

        previous = Path.cwd()
        try:
            os.chdir(root.parent)
            relative_catalog = load_catalog("catalog")
        finally:
            os.chdir(previous)
        self.assertEqual(catalog["manifests"], relative_catalog["manifests"])

    def test_malformed_root_spelling_has_the_fixed_filesystem_error(self):
        self._assert_filesystem_error(str(self.base / "catalog") + "\x00")

    def test_root_ancestor_owner_module_and_manifest_links_are_rejected(self):
        real_root = self._catalog("root-link-real")
        root_link = self.base / "root-link"
        root_link.symlink_to(real_root, target_is_directory=True)
        self._assert_filesystem_error(root_link)

        real_parent = self.base / "ancestor-real"
        ancestor_root = real_parent / "catalog"
        self._write_manifest(ancestor_root, "portunex", "identity", b"{}")
        ancestor_link = self.base / "ancestor-link"
        ancestor_link.symlink_to(real_parent, target_is_directory=True)
        self._assert_filesystem_error(ancestor_link / "catalog")

        owner_root = self.base / "owner-link-root"
        owner_root.mkdir()
        owner_target = self.base / "owner-link-target"
        self._write_manifest(owner_target.parent, owner_target.name, "identity", b"{}")
        (owner_root / "portunex").symlink_to(owner_target, target_is_directory=True)
        self._assert_filesystem_error(owner_root)

        module_root = self.base / "module-link-root"
        (module_root / "portunex").mkdir(parents=True)
        module_target = self.base / "module-link-target"
        self._write_manifest(module_target.parent, module_target.name, "unused", b"{}")
        # Keep only the module directory that will be linked under the valid
        # owner; the target's own nested fixture is irrelevant to loading.
        target_module = module_target / "unused"
        (module_root / "portunex" / "identity").symlink_to(target_module, target_is_directory=True)
        self._assert_filesystem_error(module_root)

        manifest_root = self.base / "manifest-link-root"
        module = manifest_root / "portunex" / "identity"
        module.mkdir(parents=True)
        outside = self.base / "outside-sentinel.json"
        sentinel = "outside-sentinel-must-not-appear"
        outside.write_text('{"marker":"%s"}' % sentinel, encoding="utf-8")
        (module / "manifest.json").symlink_to(outside)
        self._assert_filesystem_error(manifest_root, sentinel)

    @unittest.skipUnless(hasattr(os, "mkfifo"), "POSIX FIFO support is required")
    def test_fifo_manifest_is_rejected_before_any_blocking_open(self):
        root = self._catalog("fifo")
        manifest = root / "portunex" / "identity" / "manifest.json"
        manifest.unlink()
        os.mkfifo(manifest)
        real_open = os.open
        opened_manifest = []

        def guarded_open(path, flags, mode=0o777, *, dir_fd=None):
            if path == "manifest.json":
                opened_manifest.append(True)
                raise AssertionError("FIFO must be rejected before open")
            if dir_fd is None:
                return real_open(path, flags, mode)
            return real_open(path, flags, mode, dir_fd=dir_fd)

        with patch("recoverykit.contracts.filesystem.os.open", side_effect=guarded_open):
            self._assert_filesystem_error(root)
        self.assertEqual([], opened_manifest)

    def test_manifest_swap_after_open_is_detected_and_never_reads_the_link_target(self):
        root = self._catalog("swap")
        manifest = root / "portunex" / "identity" / "manifest.json"
        outside = self.base / "outside-after-inventory.json"
        sentinel = "outside-after-inventory-sentinel"
        outside.write_text('{"marker":"%s"}' % sentinel, encoding="utf-8")
        real_open = os.open
        swapped = []

        def open_then_swap(path, flags, mode=0o777, *, dir_fd=None):
            if dir_fd is None:
                descriptor = real_open(path, flags, mode)
            else:
                descriptor = real_open(path, flags, mode, dir_fd=dir_fd)
            if path == "manifest.json" and not swapped:
                swapped.append(True)
                manifest.unlink()
                manifest.symlink_to(outside)
            return descriptor

        with patch("recoverykit.contracts.filesystem.os.open", side_effect=open_then_swap):
            self._assert_filesystem_error(root, sentinel)
        self.assertEqual([True], swapped)

    def test_manifest_change_while_its_fd_is_read_is_detected(self):
        root = self._catalog("unstable")
        manifest = root / "portunex" / "identity" / "manifest.json"
        real_read = os.read
        changed = []

        def read_then_mutate(descriptor, count):
            if not changed:
                changed.append(True)
                information = manifest.stat()
                os.utime(
                    manifest,
                    ns=(information.st_atime_ns, information.st_mtime_ns + 1_000_000_000),
                )
            return real_read(descriptor, count)

        with patch("recoverykit.contracts.filesystem.os.read", side_effect=read_then_mutate):
            self._assert_filesystem_error(root)
        self.assertEqual([True], changed)

    def test_manifest_total_count_and_directory_budgets_fail_closed(self):
        oversize = self._catalog("oversize", raw=b"x" * (catalog_filesystem.MAX_MANIFEST_BYTES + 1))
        self._assert_limit_error(oversize)

        too_many = self._catalog(
            "too-many",
            modules=tuple("module%03d" % index for index in range(catalog_filesystem.MAX_MANIFESTS + 1)),
        )
        self._assert_limit_error(too_many)

        aggregate_raw = b'{"payload":"' + (b"x" * 64) + b'"}'
        aggregate = self._catalog("aggregate", modules=("first", "second"), raw=aggregate_raw)
        with patch(
            "recoverykit.contracts.loader.MAX_CATALOG_BYTES",
            len(aggregate_raw) * 2 - 1,
        ):
            self._assert_limit_error(aggregate)

        enumeration = self._catalog("enumeration", modules=("first", "second"))
        with patch("recoverykit.contracts.filesystem.MAX_DIRECTORY_ENTRIES", 1):
            self._assert_limit_error(enumeration)


if __name__ == "__main__":
    unittest.main()
