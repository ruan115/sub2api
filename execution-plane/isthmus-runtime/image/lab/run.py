"""Manual, phased S1b experiment on the one approved test daemon.

Use root-owned /var/tmp/isthmus-s1b.<random> and review each phase's evidence.
This is not a production installer. No credentials or real CLI are accepted.
"""
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import time

ROOT = Path(__file__).resolve().parents[4]
sys.path[:0] = [str(ROOT / "recovery/tooling"), str(Path(__file__).resolve().parents[1])]
from imagekit.context import stage_context, verify_context
from imagekit.lock import validate_lock
from lab.host import BASE, BUILDKIT, LABEL, Lab, validate_builder

CORE = ["ca-certificates", "passwd", "procps", "util-linux"]
HERE = Path(__file__).resolve().parent


def read(lab, name):
    return json.loads((lab.root / name).read_text())


def prepare(lab):
    lab.save("baseline.json", lab.baseline())
    for name, ref in (("pull-base", BASE), ("pull-builder", BUILDKIT)):
        lab.logged(name, ["pull", "--platform", "linux/amd64", ref], timeout=600)
        item = lab.json("image", "inspect", ref)[0]
        if item["Os"] != "linux" or item["Architecture"] != "amd64":
            raise ValueError("pulled_image_architecture_mismatch")
        lab.save(name + "-image.json", {k: item[k] for k in ("Id", "RepoDigests", "Os", "Architecture")})
    cid = lab.create_probe("prepare", BASE, ["/bin/sleep", "1800"], network="bridge", memory="512m")
    lab.owned(cid)
    lab.save("prepare-container.json", {"id": cid})
    apt(lab)


def apt(lab):
    cid = read(lab, "prepare-container.json")["id"]
    lab.owned(cid)
    lab.run("cp", str(HERE / "debian.sources"), cid + ":/etc/apt/sources.list.d/debian.sources")
    lab.run("start", cid)
    lab.logged("apt-update", ["exec", cid, "apt-get", "-o", "Acquire::Retries=0",
               "-o", "Acquire::http::Timeout=20", "-o", "Debug::Acquire::gpgv=true",
               "--error-on=any", "update"], timeout=300, stop_on_abort=cid)
    lab.logged("apt-plan", ["exec", cid, "apt-get", "--simulate", "--reinstall",
               "--no-install-recommends", "--no-remove", "install", *CORE], timeout=30,
               stop_on_abort=cid)
    print(json.dumps({"phase": "prepare", "status": "review_apt_plan_before_download"}))


def download(lab):
    cid = read(lab, "prepare-container.json")["id"]
    lab.owned(cid)
    plan = (lab.root / "apt-plan.log").read_text()
    if "Remv " in plan or "WARNING: The following packages cannot be authenticated" in plan:
        raise ValueError("apt_plan_rejected")
    lab.logged("apt-download", ["exec", cid, "apt-get", "-o", "Acquire::Retries=0",
               "-o", "Acquire::http::Timeout=20", "--yes", "--download-only", "--reinstall",
               "--no-install-recommends", "--no-remove", "install", *CORE], timeout=300,
               stop_on_abort=cid)
    # Only official public APT evidence from our own fresh container.
    for source, name in (("/var/cache/apt/archives", "archives"),
                         ("/var/lib/apt/lists", "apt-lists"),
                         ("/usr/share/keyrings/debian-archive-keyring.pgp", "archive-keyring.pgp")):
        lab.run("cp", cid + ":" + source, str(lab.root / name), timeout=60)
    collect(lab)


def collect(lab):
    from lab.packages import build_package
    cid = read(lab, "prepare-container.json")["id"]
    lab.owned(cid)
    if not (lab.root / "archive-keyring.pgp").is_file() or (lab.root / "archive-keyring.pgp").is_symlink():
        raise ValueError("archive_keyring_not_regular")
    packages = []
    files = sorted((lab.root / "archives").glob("*.deb"))
    if not 4 <= len(files) <= 128:
        raise ValueError("package_count_rejected")
    target = lab.root / "packages"
    target.mkdir(mode=0o700)
    for path in files:
        source = "/var/cache/apt/archives/" + path.name
        control = lab.run("exec", cid, "dpkg-deb", "--field", source,
                          "Package", "Version", "Architecture").stdout
        fields = dict(line.split(": ", 1) for line in control.strip().splitlines())
        record = lab.run("exec", cid, "apt-cache", "show", "--no-all-versions",
                         fields["Package"] + "=" + fields["Version"]).stdout.strip()
        # The signed index record determines the canonical pool path. For a
        # security-origin version, query the actual indexed URI (not MD5).
        uris = lab.run("exec", cid, "apt-get", "--print-uris", "download",
                       fields["Package"] + "=" + fields["Version"]).stdout
        lines = [line for line in uris.splitlines() if line.startswith("'")]
        if len(lines) != 1:
            raise ValueError("apt_uri_count_mismatch")
        match = re.fullmatch(r"'([^']+)' (\S+) ([0-9]+) (?:[A-Za-z0-9]+:)?[a-fA-F0-9]+", lines[0])
        if not match:
            raise ValueError("apt_uri_format_rejected")
        uri, _, uri_size = match.groups()
        if uri.startswith("http://security.debian.org/debian-security/pool/"):
            origin = "https://security.debian.org/debian-security/"
        elif uri.startswith("http://deb.debian.org/debian/pool/"):
            origin = "https://deb.debian.org/debian/"
        else:
            raise ValueError("apt_package_origin_rejected")
        if path.stat().st_size > 64 * 1024**2:
            raise ValueError("package_size_limit")
        digest = hashlib.sha256(path.read_bytes()).hexdigest()
        item = build_package(record, control, path.stat().st_size, digest, origin)
        # APT percent-encodes literal '+' in versioned filenames; compare its
        # exact public location, without permitting encoded separators/traversal.
        canonical_uri = re.sub(r"%2[bB]", "+", re.sub(r"%7[eE]", "~", uri))
        if canonical_uri.replace("http://", "https://", 1) != item["source_url"] or int(uri_size) != item["size"]:
            raise ValueError("apt_uri_metadata_mismatch")
        lab.save("package-" + item["name"] + ".json", {"index_record": record,
                 "control": control, "print_uris": uris, "sha256": digest})
        cache_name = fields["Package"] + "_" + fields["Version"].replace(":", "%3a") + "_" + fields["Architecture"] + ".deb"
        if path.name not in (item["file"], cache_name):
            raise ValueError("downloaded_filename_mismatch")
        path.rename(target / item["file"])
        (target / item["file"]).chmod(0o600)
        packages.append(item)
    lock = validate_lock({"schema_version": 1, "kind": "isthmus-base-build-inputs",
                          "platform": "linux/amd64", "base_image": BASE, "packages": packages})
    lab.save("base.lock.json", lock)
    evidence = []
    for p in sorted((lab.root / "apt-lists").iterdir()):
        if p.is_file() and p.name != "lock":
            evidence.append({"file": p.name, "size": p.stat().st_size,
                             "sha256": hashlib.sha256(p.read_bytes()).hexdigest()})
    lab.save("apt-evidence.json", {"files": evidence, "transport": "HTTP with signed APT indexes",
             "source_urls": "HTTPS locations of matching official pool bytes",
             "keyring_sha256": hashlib.sha256((lab.root / "archive-keyring.pgp").read_bytes()).hexdigest(),
             "trust_root": "debian-archive-keyring from pinned official base digest"})
    lab.run("stop", "--time", "5", cid)
    lab.save("stage.json", stage_context(lab.root / "base.lock.json", target, lab.root / "context"))
    lab.save("verify.json", verify_context(lab.root / "context"))
    print(json.dumps({"phase": "download", "packages": len(packages),
                      "status": "signed_apt_download_and_context_verified"}))


def main():
    os.umask(0o077)
    from lab.build import build
    from lab.smoke import smoke
    from lab.cleanup import cleanup
    if len(sys.argv) != 3 or sys.argv[1] not in ("prepare", "apt", "download", "build", "smoke", "cleanup"):
        raise ValueError("explicit_phase_and_root_required")
    lab = Lab(sys.argv[2])
    {"prepare": prepare, "apt": apt, "download": download, "build": build, "smoke": smoke, "cleanup": cleanup}[sys.argv[1]](lab)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"ok": False, "error_type": type(error).__name__,
                          "reason": str(error) if str(error).replace("_", "").replace(":", "").replace("-", "").isalnum() else "details_retained_in_private_lab"}), file=sys.stderr)
        raise SystemExit(2)
