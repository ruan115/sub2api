"""Synthetic ZIP/ELF and metadata; never execute or download a toolchain."""
import copy
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import struct
import tempfile
import unittest
from unittest.mock import patch
import warnings
import zipfile

from artifacts import binaries as module


def elf(machine=62):
    header = bytearray(64)
    header[:7] = b"\x7fELF\x02\x01\x01"
    struct.pack_into("<HHI", header, 16, 3, machine, 1)
    struct.pack_into("<H", header, 52, 64)
    return bytes(header) + b"synthetic-not-executable" * 64


def archive(payload=None, *, member="bun-linux-x64/bun", directory=False, extra=None,
            mode=stat.S_IFREG | 0o755, compression=zipfile.ZIP_DEFLATED):
    out = io.BytesIO()
    with zipfile.ZipFile(out, "w", compression=compression) as zipped:
        if directory:
            info = zipfile.ZipInfo(member.rsplit("/", 1)[0] + "/")
            info.create_system = 3
            info.external_attr = (stat.S_IFDIR | 0o755) << 16
            zipped.writestr(info, b"")
        info = zipfile.ZipInfo(member)
        info.create_system = 3
        info.external_attr = mode << 16
        info.compress_type = compression
        zipped.writestr(info, elf() if payload is None else payload)
        if extra:
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", UserWarning)
                zipped.writestr(extra, b"unexpected")
    return out.getvalue()


def spec(payload, *, name="bun", version="1.4.2", platform="linux/amd64"):
    arch = {"linux/amd64": "x64", "linux/arm64": "aarch64"}[platform]
    if name == "bun":
        return {"name": name, "role": "candidate" if version == "1.4.2" else "control",
                "version": version, "platform": platform, "format": "zip",
                "file": f"bun-{version}-linux-{arch}.zip",
                "source_url": f"https://github.com/oven-sh/bun/releases/download/bun-v{version}/bun-linux-{arch}.zip",
                "size": len(payload), "sha256": hashlib.sha256(payload).hexdigest(),
                "member": f"bun-linux-{arch}/bun"}
    arch = "x64" if platform == "linux/amd64" else "arm64"
    return {"name": name, "role": "cli", "version": "2.1.258", "platform": platform,
            "format": "elf", "file": f"claude-2.1.258-linux-{arch}",
            "source_url": f"https://downloads.claude.ai/claude-code-releases/2.1.258/linux-{arch}/claude",
            "size": len(payload), "sha256": hashlib.sha256(payload).hexdigest(), "member": None}


class BinaryLockTests(unittest.TestCase):
    def fixture(self):
        return json.loads((Path(__file__).resolve().parents[1] / "locks/toolchain-linux-2026-09-17.json").read_text())

    def test_actual_public_lock_has_exact_six_versions_and_canonical_copy(self):
        document = self.fixture()
        result = module.validate_lock(document)
        self.assertEqual(len(result["artifacts"]), 6)
        self.assertEqual([item["file"] for item in result["artifacts"]], sorted(item["file"] for item in document["artifacts"]))
        document["artifacts"][0]["sha256"] = "changed"
        self.assertNotIn("changed", json.dumps(result))
        self.assertNotIn("pgp_verified", result)

    def test_missing_duplicate_extra_and_malformed_fields_are_rejected(self):
        source = self.fixture()
        for change in ({"schema_version": True}, {"kind": "runtime"}, {"extra": 1},
                       {"artifacts": source["artifacts"][:-1]},
                       {"artifacts": source["artifacts"][:-1] + source["artifacts"][:1]},
                       {"artifacts": source["artifacts"] * 2}, {"artifacts": None}):
            with self.subTest(change=list(change)), self.assertRaises(module.ArtifactError):
                module.validate_lock(dict(source, **change))
        for field, value in (("size", True), ("size", 0), ("size", 256 * 1024**2 + 1),
                             ("sha256", "A" * 64), ("sha256", []), ("member", "../bun"),
                             ("version", "latest"), ("role", "cli"), ("format", "elf"),
                             ("platform", "linux/386"), ("file", "../bun.zip"),
                             ("name", "node"), ("source_url", "https://github.com/oven-sh/bun/releases/latest/download/bun-linux-x64.zip")):
            document = self.fixture()
            document["artifacts"][0][field] = value
            with self.subTest(field=field), self.assertRaises(module.ArtifactError):
                module.validate_lock(document)
        for field in source["artifacts"][0]:
            document = self.fixture()
            del document["artifacts"][0][field]
            with self.subTest(missing=field), self.assertRaises(module.ArtifactError):
                module.validate_lock(document)


class BinaryStagingTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.root.chmod(0o700)
        self.input = self.root / "input"
        self.dest = self.root / "bun"

    def stage(self, payload, *, metadata=None):
        self.input.write_bytes(payload)
        return module.stage_binary(self.input, spec(payload) if metadata is None else metadata, self.dest)

    def test_zip_with_or_without_only_empty_parent_is_streamed_to_private_new_binary(self):
        for directory in (False, True):
            with self.subTest(directory=directory):
                self.dest = self.root / ("bun-with-directory" if directory else "bun-single")
                payload = archive(directory=directory)
                with patch.object(module, "_CHUNK", 17), \
                     patch("subprocess.run", side_effect=AssertionError("unexpected execution")), \
                     patch("socket.socket", side_effect=AssertionError("unexpected network")):
                    receipt = self.stage(payload)
                self.assertEqual(self.dest.read_bytes(), elf())
                self.assertEqual(stat.S_IMODE(self.dest.stat().st_mode), 0o755)
                self.assertEqual(receipt["size"], len(elf()))
                self.assertEqual(receipt["sha256"], hashlib.sha256(elf()).hexdigest())
                self.assertEqual(receipt["source_sha256"], hashlib.sha256(payload).hexdigest())
                self.assertEqual(receipt["status"], "binary_staged")
                self.assertFalse(receipt["execution_permitted"])
                self.assertFalse(receipt["image_built"])
                self.assertNotIn(str(self.root), json.dumps(receipt))

    def test_raw_cli_and_arm64_elf_are_checked(self):
        for name in ("bun", "claude"):
            for platform, machine in (("linux/amd64", 62), ("linux/arm64", 183)):
                raw = elf(machine)
                member = "bun-linux-x64/bun" if machine == 62 else "bun-linux-aarch64/bun"
                payload = archive(raw, member=member) if name == "bun" else raw
                self.dest = self.root / (name + str(machine))
                result = self.stage(payload, metadata=spec(payload, name=name, platform=platform))
                self.assertEqual(self.dest.read_bytes(), raw)
                self.assertEqual(result["platform"], platform)

    def test_wrong_outer_hash_or_size_fails_before_destination_creation(self):
        payload = archive()
        for field, value in (("sha256", "0" * 64), ("size", len(payload) + 1)):
            metadata = spec(payload)
            metadata[field] = value
            with self.subTest(field=field), self.assertRaises(module.ArtifactError):
                self.stage(payload, metadata=metadata)
            self.assertFalse(self.dest.exists())

    def test_extra_duplicate_traversal_absolute_and_wrong_arch_entries_rejected(self):
        for payload in (archive(extra="unexpected"), archive(extra="bun-linux-x64/bun"),
                        archive(member="../bun"), archive(member="/bun-linux-x64/bun"),
                        archive(member="bun-linux-aarch64/bun"),
                        archive(member="bun-linux-x64/../bun"), archive(member="bun-linux-x64\\bun")):
            with self.subTest(size=len(payload)), self.assertRaises(module.ArtifactError):
                self.stage(payload)
            self.assertFalse(self.dest.exists())

    def test_links_special_encryption_unknown_compression_and_nonempty_parent_rejected(self):
        values = [archive(mode=kind | 0o755) for kind in (stat.S_IFLNK, stat.S_IFIFO, stat.S_IFCHR, stat.S_IFSOCK)]
        values.append(archive(compression=zipfile.ZIP_BZIP2))
        encrypted = bytearray(archive())
        central = encrypted.index(b"PK\x01\x02")
        struct.pack_into("<H", encrypted, 6, 1)
        struct.pack_into("<H", encrypted, central + 8, 1)
        values.append(bytes(encrypted))
        out = io.BytesIO()
        with zipfile.ZipFile(out, "w") as z:
            z.writestr("bun-linux-x64/", b"not-empty")
            z.writestr("bun-linux-x64/bun", elf())
        values.append(out.getvalue())
        for payload in values:
            with self.subTest(size=len(payload)), self.assertRaises(module.ArtifactError):
                self.stage(payload)
            self.assertFalse(self.dest.exists())

    def test_bad_elf_class_endian_machine_and_header_fail_before_output(self):
        for offset, value in ((0, 0), (4, 1), (5, 2), (6, 0), (18, 183), (52, 0)):
            malformed = bytearray(elf())
            malformed[offset] = value
            with self.subTest(offset=offset), self.assertRaises(module.ArtifactError):
                self.stage(archive(bytes(malformed)))
            self.assertFalse(self.dest.exists())

    def test_archive_directory_count_size_and_binary_expansion_are_bounded_before_write(self):
        payload = archive()
        for offset, value, fmt in ((10, 3, "H"), (12, 9000, "I"), (16, 0xFFFFFFFF, "I")):
            changed = bytearray(payload)
            end = changed.rindex(b"PK\x05\x06")
            struct.pack_into("<" + fmt, changed, end + offset, value)
            with self.subTest(offset=offset), patch.object(module.zipfile, "ZipFile") as constructor:
                with self.assertRaises(module.ArtifactError):
                    self.stage(bytes(changed))
                constructor.assert_not_called()
            self.assertFalse(self.dest.exists())
        with patch.object(module, "MAX_BINARY_BYTES", 128), self.assertRaises(module.ArtifactError):
            self.stage(payload)
        self.assertFalse(self.dest.exists())

    def test_nul_name_and_concatenated_archives_cannot_hide_extra_data(self):
        payload = archive(member="bun-linux-x64/bun/hidden")
        # Keep both header name lengths intact; Python otherwise truncates NUL
        # when *creating* ZIPs and would not actually test the read-side guard.
        payload = payload.replace(b"bun-linux-x64/bun/hidden", b"bun-linux-x64/bun\x00hidden")
        for malformed in (payload, archive() + archive(), archive()[:-1]):
            with self.subTest(size=len(malformed)), self.assertRaises(module.ArtifactError):
                self.stage(malformed)
            self.assertFalse(self.dest.exists())

    def test_crc_damage_cannot_return_success_or_publish_executable(self):
        changed = bytearray(archive(compression=zipfile.ZIP_STORED))
        start = changed.index(b"\x7fELF")
        changed[start + 100] ^= 1
        with self.assertRaises(module.ArtifactError):
            self.stage(bytes(changed))
        if self.dest.exists():
            self.assertEqual(stat.S_IMODE(self.dest.stat().st_mode), 0o600)

    def test_source_symlinks_fifo_and_hardlinks_are_rejected(self):
        payload = archive()
        real = self.root / "real"
        real.write_bytes(payload)
        for kind in ("symlink", "hardlink", "fifo"):
            self.input = self.root / kind
            if kind == "symlink":
                self.input.symlink_to(real)
            elif kind == "hardlink":
                os.link(real, self.input)
            else:
                os.mkfifo(self.input)
            with self.subTest(kind=kind), self.assertRaises(module.ArtifactError):
                module.stage_binary(self.input, spec(payload), self.dest)
            self.assertFalse(self.dest.exists())

    def test_relative_paths_existing_output_symlink_parent_and_git_output_fail(self):
        payload = archive()
        self.input.write_bytes(payload)
        for source, destination in ((Path("relative"), self.dest), (self.input, Path("relative"))):
            with self.assertRaises(module.ArtifactError):
                module.stage_binary(source, spec(payload), destination)
        self.dest.write_bytes(b"preserve-user-data")
        with self.assertRaises(module.ArtifactError):
            module.stage_binary(self.input, spec(payload), self.dest)
        self.assertEqual(self.dest.read_bytes(), b"preserve-user-data")
        link = self.root / "parent-link"
        link.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(module.ArtifactError):
            module.stage_binary(self.input, spec(payload), link / "new")
        (self.root / ".git").mkdir()
        with self.assertRaises(module.ArtifactError):
            module.stage_binary(self.input, spec(payload), self.root / "new")

    def test_parent_must_be_owner_only_and_source_mutation_is_rejected(self):
        payload = archive()
        self.root.chmod(0o755)
        with self.assertRaises(module.ArtifactError):
            self.stage(payload)
        self.assertFalse(self.dest.exists())
        self.root.chmod(0o700)
        original = module._zip_member
        def mutate(reader, size, metadata):
            result = original(reader, size, metadata)
            self.input.write_bytes(payload + b"changed-after-verification")
            return result
        with patch.object(module, "_zip_member", side_effect=mutate), self.assertRaises(module.ArtifactError):
            self.stage(payload)
        self.assertFalse(self.dest.exists())

    def test_same_byte_source_path_replacement_after_copy_is_rejected(self):
        payload = archive()
        original = module._copy_binary
        def replace_input(*args):
            result = original(*args)
            replacement = self.root / "replacement"
            replacement.write_bytes(payload)
            os.replace(replacement, self.input)
            return result
        with patch.object(module, "_copy_binary", side_effect=replace_input), \
             self.assertRaisesRegex(module.ArtifactError, "binary_file_type_rejected"):
            self.stage(payload)
        self.assertEqual(stat.S_IMODE(self.dest.stat().st_mode), 0o600)

    def test_partial_write_and_zero_write_are_handled_without_success(self):
        payload = archive()
        original = os.write
        with patch.object(module.os, "write", side_effect=lambda fd, data: original(fd, data[:11])):
            self.stage(payload)
        self.assertEqual(self.dest.read_bytes(), elf())
        self.dest = self.root / "failed"
        with patch.object(module.os, "write", return_value=0), self.assertRaises(module.ArtifactError):
            self.stage(payload)
        self.assertEqual(stat.S_IMODE(self.dest.stat().st_mode), 0o600)

    def test_content_change_during_executable_chmod_cannot_return_stale_receipt(self):
        original = os.fchmod
        def change_after_chmod(fd, mode):
            original(fd, mode)
            if mode == 0o755:
                os.pwrite(fd, b"X", 0)
        for name in ("bun", "claude"):
            self.dest = self.root / name
            payload = archive() if name == "bun" else elf()
            with self.subTest(name=name), \
                 patch.object(module.os, "fchmod", side_effect=change_after_chmod), \
                 self.assertRaisesRegex(module.ArtifactError, "binary_output_changed"):
                self.stage(payload, metadata=spec(payload, name=name))
            self.assertNotEqual(self.dest.read_bytes(), elf())


if __name__ == "__main__":
    unittest.main()
