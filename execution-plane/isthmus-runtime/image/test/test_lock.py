import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from recoverykit.evidence.errors import EvidenceError
from imagekit.lock import ImageError, load_lock, validate_lock


def fixture():
    payload = b"!<arch>\nsynthetic-not-an-installable-package"
    packages = []
    for name in ("ca-certificates", "passwd", "procps", "util-linux"):
        filename = f"{name}_1.0_all.deb"
        packages.append({"name": name, "version": "1.0", "architecture": "all", "file": filename,
                         "size": len(payload), "sha256": hashlib.sha256(payload).hexdigest(),
                         "source_url": f"https://deb.debian.org/debian/pool/main/p/pkg/{filename}"})
    return {"schema_version": 1, "kind": "isthmus-base-build-inputs", "platform": "linux/amd64",
            "base_image": "docker.io/library/debian@sha256:" + "a" * 64, "packages": packages}


class LockTests(unittest.TestCase):
    def test_complete_lock_is_sorted_copied_and_architecture_bound(self):
        document = fixture()
        document["packages"].reverse()
        result = validate_lock(document)
        self.assertEqual([p["file"] for p in result["packages"]], sorted(p["file"] for p in document["packages"]))
        document["packages"][0]["name"] = "changed"
        self.assertNotIn("changed", [p["name"] for p in result["packages"]])
        for architecture in ("arm64", "amd64"):
            document = fixture()
            document["platform"] = "linux/" + architecture
            for package in document["packages"]:
                package["architecture"] = architecture
                package["file"] = package["file"].replace("_all.deb", "_" + architecture + ".deb")
                package["source_url"] = package["source_url"].replace("_all.deb", "_" + architecture + ".deb")
            self.assertEqual(validate_lock(document), document)

    def test_root_fields_and_base_must_be_exact(self):
        cases = [None, [], {}, dict(fixture(), schema_version=True), dict(fixture(), platform=[]),
                 dict(fixture(), schema_version=2), dict(fixture(), kind="runtime"),
                 dict(fixture(), platform="linux/386"), dict(fixture(), extra=True)]
        cases.extend(dict(fixture(), base_image=base) for base in (
            "debian:13", "debian:latest", "sha256:" + "a" * 64,
            "docker.io/library/debian@sha256:" + "A" * 64,
            "docker.io/library/debian@sha256:" + "a" * 64 + "\nRUN arbitrary", "https://remote.invalid", True))
        for document in cases:
            with self.subTest(case_type=type(document).__name__), self.assertRaises(ImageError):
                validate_lock(document)

    def test_missing_extra_wrong_type_and_injection_package_fields_fail(self):
        for field in fixture()["packages"][0]:
            document = fixture()
            del document["packages"][0][field]
            with self.subTest(missing=field), self.assertRaises(ImageError):
                validate_lock(document)
        for field, values in {
            "file": ["../package.deb", "/package.deb", "a\\b.deb", "x.deb", None, "x\nRUN x"],
            "name": ["bad;id", "BAD", "$(id)", "bad name", [], "libcap2-bin"],
            "version": ["latest", "1.0\n", "1$(id)", "1;id", "1_2", "1:2:3", False],
            "architecture": ["arm64", "aarch64", None, ["all"]],
            "size": [True, 0, 7, -1, 1.0, float("nan"), float("inf"), 64 * 1024 * 1024 + 1],
            "sha256": ["a" * 63, "G" * 64, "a" * 64 + "\n", None],
        }.items():
            for value in values:
                document = fixture()
                document["packages"][0][field] = value
                with self.subTest(field=field, value_type=type(value).__name__), self.assertRaises(ImageError):
                    validate_lock(document)
        document = fixture()
        document["packages"][0]["token"] = "synthetic-private-value"
        with self.assertRaises(ImageError):
            validate_lock(document)

    def test_epoch_version_and_official_source_forms(self):
        for prefix in ("https://deb.debian.org/debian/pool/main/p/pkg/",
                       "https://security.debian.org/debian-security/pool/updates/main/p/pkg/",
                       "https://snapshot.debian.org/archive/debian/20260901T000000Z/pool/main/p/pkg/",
                       "https://snapshot.debian.org/archive/debian-security/20260901T000000Z/pool/updates/main/p/pkg/"):
            document = fixture()
            package = document["packages"][0]
            package["version"] = "2:1.0+Debian~1-1"
            package["file"] = "ca-certificates_1.0+Debian~1-1_all.deb"
            package["source_url"] = prefix + package["file"]
            self.assertEqual(validate_lock(document)["packages"][0], package)

    def test_source_urls_cannot_select_network_credentials_or_traversal(self):
        filename = fixture()["packages"][0]["file"]
        for source in ("http://deb.debian.org/debian/pool/main/p/pkg/" + filename,
                       "https://deb.debian.org:443/debian/pool/main/p/pkg/" + filename,
                       "https://" + "synthetic-user:synthetic-pass" + "@deb.debian.org/debian/pool/main/p/pkg/" + filename,
                       "https://deb.debian.org.attacker.invalid/debian/pool/main/p/pkg/" + filename,
                       "https://deb.debian.org/debian/pool/main/../" + filename,
                       "https://deb.debian.org/debian/pool/main/%2e%2e/" + filename,
                       "https://deb.debian.org/debian/pool/main//" + filename,
                       fixture()["packages"][0]["source_url"] + "?token=synthetic-private-value",
                       fixture()["packages"][0]["source_url"] + "#fragment",
                       fixture()["packages"][0]["source_url"] + "?",
                       fixture()["packages"][0]["source_url"] + "#",
                       "file:///local/" + filename, "https://snapshot.debian.org/archive/debian/latest/" + filename,
                       "https://deb.debian.org/debian/pool/main/p/pkg/wrong.deb"):
            document = fixture()
            document["packages"][0]["source_url"] = source
            with self.subTest(length=len(source)), self.assertRaises((ImageError, EvidenceError)):
                validate_lock(document)

    def test_count_total_missing_core_and_duplicate_packages_fail(self):
        for packages in ([], None, {}, fixture()["packages"][:-1], fixture()["packages"] * 2,
                         fixture()["packages"] * 33):
            with self.assertRaises(ImageError):
                validate_lock(dict(fixture(), packages=packages))
        with patch("imagekit.lock.MAX_TOTAL_BYTES", 64), self.assertRaises(ImageError):
            validate_lock(fixture())
        document = fixture()
        extra = copy.deepcopy(document["packages"][0])
        extra.update(version="2.0", file="ca-certificates_2.0_all.deb",
                     source_url="https://deb.debian.org/debian/pool/main/p/pkg/ca-certificates_2.0_all.deb")
        document["packages"].append(extra)
        with self.assertRaises(ImageError):
            validate_lock(document)

    def test_sensitive_package_is_denied_even_with_consistent_identity(self):
        document = fixture()
        package = copy.deepcopy(document["packages"][0])
        package.update(name="libcap2-bin", file="libcap2-bin_1.0_all.deb",
                       source_url="https://deb.debian.org/debian/pool/main/libc/libcap2/libcap2-bin_1.0_all.deb")
        document["packages"].append(package)
        with self.assertRaisesRegex(ImageError, "outside_base_scope"):
            validate_lock(document)

    def test_lock_loader_rejects_duplicate_json_nan_oversize_and_symlink(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            lock = root / "lock.json"
            lock.write_text(json.dumps(fixture()))
            self.assertEqual(load_lock(lock), fixture())
            link = root / "link.json"
            link.symlink_to(lock)
            with self.assertRaises(EvidenceError):
                load_lock(link)
            with self.assertRaises(EvidenceError):
                load_lock(Path("relative.json"))
            with patch("imagekit.lock.MAX_METADATA_BYTES", 1), self.assertRaises(EvidenceError):
                load_lock(lock)
            lock.write_text(json.dumps(fixture()).replace('"schema_version": 1', '"schema_version": 1, "schema_version": 1'))
            with self.assertRaises(EvidenceError):
                load_lock(lock)
            document = fixture()
            document["packages"][0]["size"] = float("nan")
            lock.write_text(json.dumps(document))
            with self.assertRaises(ImageError):
                load_lock(lock)
