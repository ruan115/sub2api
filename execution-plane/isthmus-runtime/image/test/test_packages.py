"""Synthetic metadata only: no APT, dpkg, network, archive or Docker execution."""
import hashlib
import unittest
from unittest.mock import patch

from imagekit.lock import ImageError, validate_lock
from lab.packages import build_package


ORIGIN = "https://deb.debian.org/debian/"
PAYLOAD = b"!<arch>\nsynthetic-not-an-installable-deb"
DIGEST = hashlib.sha256(PAYLOAD).hexdigest()


def metadata(name="procps", version="2:4.0.4-9", architecture="amd64"):
    filename = f"{name}_{version.split(':')[-1]}_{architecture}.deb"
    control = f"Package: {name}\nVersion: {version}\nArchitecture: {architecture}\n"
    record = (control + "Source: synthetic-source\nDepends: libc6 (>= 2.38)\n"
              + f"Filename: pool/main/p/procps/{filename}\nSize: {len(PAYLOAD)}\nSHA256: {DIGEST}\n"
              + "Description: synthetic package, not a real archive\n"
              + " continuation with UTF-8: dépendance\n .\n another paragraph\n\n")
    return record, control


def build(record=None, control=None, size=len(PAYLOAD), digest=DIGEST, origin=ORIGIN):
    default_record, default_control = metadata()
    return build_package(default_record if record is None else record,
                         default_control if control is None else control,
                         size, digest, origin)


class PackageMetadataTests(unittest.TestCase):
    def test_exact_metadata_epoch_and_hash_build_a_flat_lock_entry(self):
        result = build()
        self.assertEqual(result, {"file": "procps_4.0.4-9_amd64.deb", "name": "procps",
                                 "version": "2:4.0.4-9", "architecture": "amd64",
                                 "size": len(PAYLOAD), "sha256": DIGEST,
                                 "source_url": ORIGIN + "pool/main/p/procps/procps_4.0.4-9_amd64.deb"})

    def test_both_origins_and_architectures_satisfy_existing_lock_schema(self):
        for arch in ("amd64", "arm64"):
            packages = []
            for name in ("ca-certificates", "passwd", "procps", "util-linux"):
                package_arch = "all" if name == "ca-certificates" else arch
                record, control = metadata(name, "1.2+deb13u1", package_arch)
                origin = ORIGIN if name != "passwd" else "https://security.debian.org/debian-security/"
                packages.append(build(record, control, origin=origin))
            lock = {"schema_version": 1, "kind": "isthmus-base-build-inputs",
                    "platform": "linux/" + arch,
                    "base_image": "docker.io/library/debian@sha256:" + "a" * 64,
                    "packages": packages}
            self.assertEqual(validate_lock(lock), lock)

    def test_crlf_case_insensitive_fields_and_field_order_are_supported(self):
        record, control = metadata()
        record = record.replace("Package:", "package:").replace("Version:", "VERSION:")
        control = "architecture: amd64\r\nVERSION: 2:4.0.4-9\r\npackage: procps\r\n"
        self.assertEqual(build(record.replace("\n", "\r\n"), control), build())

    def test_each_required_index_field_and_control_field_is_required(self):
        record, control = metadata()
        for field in ("Package", "Version", "Architecture", "Filename", "Size", "SHA256"):
            malformed = "\n".join(line for line in record.split("\n") if not line.startswith(field + ":"))
            with self.subTest(index_field=field), self.assertRaises(ImageError):
                build(record=malformed)
        for field in ("Package", "Version", "Architecture"):
            malformed = "\n".join(line for line in control.split("\n") if not line.startswith(field + ":"))
            with self.subTest(control_field=field), self.assertRaises(ImageError):
                build(control=malformed)

    def test_duplicate_fields_even_identical_or_case_variant_are_rejected(self):
        record, control = metadata()
        for field in ("Package", "Version", "Architecture", "Filename", "Size", "SHA256", "Description"):
            line = next(line for line in record.split("\n") if line.startswith(field + ":"))
            for duplicate in (line, line.lower()):
                with self.subTest(field=field), self.assertRaises(ImageError):
                    build(record=duplicate + "\n" + record)
        with self.assertRaises(ImageError):
            build(control=control + "package: procps\n")

    def test_multiple_stanzas_and_folded_security_fields_are_rejected(self):
        record, control = metadata()
        for malformed in (record + record, record + "Package: other\n", "\n" + record,
                          record.replace("Package: procps", "Package: proc\n ps"),
                          record.replace("SHA256: " + DIGEST, "SHA256: " + DIGEST[:32] + "\n " + DIGEST[32:]),
                          " orphan\n" + record):
            with self.subTest(length=len(malformed)), self.assertRaises(ImageError):
                build(record=malformed)
        with self.assertRaises(ImageError):
            build(control=control + "Depends: libc6\n")

    def test_control_identity_must_match_exactly(self):
        _, control = metadata()
        for old, new in (("procps", "passwd"), ("2:4.0.4-9", "4.0.4-9"), ("amd64", "arm64")):
            with self.subTest(value=new), self.assertRaises(ImageError):
                build(control=control.replace(old, new))

    def test_measured_size_and_sha256_are_strict_and_must_match(self):
        for size in (True, False, "42", 1.0, 0, 7, len(PAYLOAD) + 1, 64 * 1024**2 + 1):
            with self.subTest(size_type=type(size).__name__), self.assertRaises(ImageError):
                build(size=size)
        for digest in (None, [], "A" * 64, "g" * 64, "0" * 64, DIGEST + "\n"):
            with self.subTest(hash_type=type(digest).__name__), self.assertRaises(ImageError):
                build(digest=digest)
        record, _ = metadata()
        for value in ("0" + str(len(PAYLOAD)), "+42", "1.0", "-1", "9" * 4000):
            with self.assertRaises(ImageError):
                build(record=record.replace("Size: " + str(len(PAYLOAD)), "Size: " + value))

    def test_invalid_package_identity_and_scope_fail_even_when_both_records_agree(self):
        for name, version, arch in (("UPPER", "1.0", "amd64"), ("bad;command", "1.0", "amd64"),
                                    ("procps", "latest", "amd64"), ("procps", "1:2:3", "amd64"),
                                    ("procps", "1.0", "i386"), ("procps", "1.0", "aarch64"),
                                    ("sudo", "1.0", "amd64"), ("libcap2-bin", "1.0", "amd64")):
            record, control = metadata(name, version, arch)
            with self.subTest(name=name, arch=arch), self.assertRaises(ImageError):
                build(record, control)

    def test_filename_is_canonical_and_only_main_pool_paths_are_allowed(self):
        record, _ = metadata()
        original = "pool/main/p/procps/procps_4.0.4-9_amd64.deb"
        for path in ("/" + original, "https://deb.debian.org/debian/" + original,
                     original.replace("pool/main", "pool/non-free"),
                     original.replace("/p/procps/", "/../"), original.replace("/p/", "//p/"),
                     original.replace("4.0.4", "2:4.0.4"), original.replace("4.0.4", "2%3a4.0.4"),
                     original.replace("amd64.deb", "all.deb"), original + "?", original + "#",
                     "pool/main/p/procps/../../procps_4.0.4-9_amd64.deb"):
            with self.subTest(length=len(path)), self.assertRaises(ImageError):
                build(record=record.replace(original, path))

    def test_origin_is_exact_https_root_without_credentials_queries_or_aliases(self):
        for origin in (None, [], ORIGIN[:-1], ORIGIN + "pool/main/", ORIGIN + "?", ORIGIN + "#",
                       "http://deb.debian.org/debian/", "https://deb.debian.org:443/debian/",
                       "https://" + "u:p" + "@deb.debian.org/debian/", "https://deb.debian.org/debian-security/",
                       "https://deb.debian.org.attacker.invalid/debian/",
                       "https://snapshot.debian.org/archive/debian/20260917T000000Z/"):
            with self.subTest(origin_type=type(origin).__name__), self.assertRaises(ImageError):
                build(origin=origin)

    def test_metadata_bounds_malformed_lines_controls_and_non_string_fail(self):
        record, control = metadata()
        for value in (None, b"Package: procps", {}, "", "\x00" + record, "\x0b" + record,
                      record.replace("\n", "\r"), "\ud800" + record,
                      "No-colon\n" + record, "Bad key: x\n" + record,
                      "Description: " + "x" * (256 * 1024),
                      "Description: " + "x" * (16 * 1024 + 1) + "\n" + record):
            with self.subTest(value_type=type(value).__name__), self.assertRaises(ImageError):
                build_package(value, control, len(PAYLOAD), DIGEST, ORIGIN)
        with self.assertRaises(ImageError):
            build(control=control + " " * (4 * 1024))
        with self.assertRaises(ImageError):
            build(record=record.rstrip("\n") + "\n" + "\n".join(f"X-{n}: x" for n in range(129)))

    def test_parser_does_not_open_files_execute_commands_or_connect_and_errors_do_not_echo_input(self):
        with patch("builtins.open", side_effect=AssertionError("unexpected I/O")), \
             patch("subprocess.run", side_effect=AssertionError("unexpected execution")), \
             patch("socket.socket", side_effect=AssertionError("unexpected network")):
            self.assertEqual(build()["sha256"], DIGEST)
        marker = "synthetic-private-input-not-to-echo"
        with self.assertRaises(ImageError) as caught:
            build(record="Package: " + marker)
        self.assertNotIn(marker, str(caught.exception))


if __name__ == "__main__":
    unittest.main()
