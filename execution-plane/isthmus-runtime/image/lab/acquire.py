"""Public-only S1c acquisition; no installer, ambient proxy, auth or execution.

Run explicitly on the reviewed lab host. The release lock is an input integrity
anchor; Claude additionally requires a detached signature with the documented key.
"""
import hashlib
from contextlib import contextmanager
import json
import os
from pathlib import Path
import time
import signal
import urllib.request

from artifacts.binaries import validate_lock, stage_binary
from lab.host import bounded_process, environment

CLAUDE_ROOT = "https://downloads.claude.ai/claude-code-releases/2.1.258/"
FINGERPRINT = "31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE"


@contextmanager
def download_deadline(seconds=300):
    """Main-thread POSIX watchdog also interrupts a slow blocked HTTPS read."""
    if signal.getitimer(signal.ITIMER_REAL) != (0.0, 0.0):
        raise ValueError("existing_process_timer_rejected")
    def expired(signum, frame):
        raise TimeoutError("download_deadline")
    previous = signal.signal(signal.SIGALRM, expired)
    try:
        signal.setitimer(signal.ITIMER_REAL, seconds)
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        parsed = urllib.parse.urlsplit(newurl)
        if (parsed.scheme != "https" or parsed.hostname not in
                {"github.com", "release-assets.githubusercontent.com", "downloads.claude.ai"}
                or parsed.username or parsed.password or parsed.port not in (None, 443)):
            raise ValueError("download_redirect_rejected")
        return super().redirect_request(request, fp, code, msg, headers, newurl)


def fetch(url, dest, limit, *, expected_size=None, expected_sha=None):
    # Callers supply only literal metadata URLs or URLs accepted by the lock.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), HTTPSRedirect())
    digest, size, start = hashlib.sha256(), 0, time.monotonic()
    with download_deadline(), opener.open(url, timeout=20) as response, Path(dest).open("xb") as out:
        os.chmod(out.fileno(), 0o600)
        while True:
            if time.monotonic() - start > 300:
                raise TimeoutError("download_deadline")
            data = response.read(min(1024 * 1024, limit + 1 - size))
            if time.monotonic() - start > 300:
                raise TimeoutError("download_deadline")
            if not data:
                break
            size += len(data)
            if size > limit:
                raise ValueError("download_size_limit")
            out.write(data)
            digest.update(data)
    if ((expected_size is not None and size != expected_size)
            or (expected_sha is not None and digest.hexdigest() != expected_sha)):
        raise ValueError("download_integrity_mismatch")
    return {"size": size, "sha256": digest.hexdigest(), "source_url": url}


def acquire(lab, lock_path):
    lock = validate_lock(json.loads(Path(lock_path).read_text()))
    root = lab.root / "toolchain-inputs"
    root.mkdir(mode=0o700)
    binaries = lab.root / "toolchain-bin"
    binaries.mkdir(mode=0o700)
    records = {}
    for filename, url in (("manifest.json", CLAUDE_ROOT + "manifest.json"),
                          ("manifest.json.sig", CLAUDE_ROOT + "manifest.json.sig"),
                          ("claude-code.asc", "https://downloads.claude.ai/keys/claude-code.asc")):
        records[filename] = fetch(url, root / filename, 256 * 1024)
    keydir = root / "gnupg"
    keydir.mkdir(mode=0o700)
    def gpg(*args):
        value = bounded_process(["/usr/bin/gpg", "--no-options", "--batch", "--homedir", str(keydir),
                                 *args], environment(lab.config), 20, 256 * 1024)
        if value.returncode:
            raise ValueError("public_key_operation_failed")
        return value.stdout
    key = gpg("--with-colons", "--show-keys", str(root / "claude-code.asc"))
    fingerprints = [line.split(":")[9] for line in key.splitlines() if line.startswith("fpr:")]
    if not fingerprints or fingerprints[0] != FINGERPRINT:
        raise ValueError("release_key_fingerprint_mismatch")
    gpg("--dearmor", "--output", str(root / "release-keyring.gpg"), str(root / "claude-code.asc"))
    result = bounded_process(["/usr/bin/gpgv", "--homedir", str(keydir), "--keyring",
        str(root / "release-keyring.gpg"), "--status-fd", "1", str(root / "manifest.json.sig"),
        str(root / "manifest.json")], environment(lab.config), 20, 256 * 1024)
    signatures = [line.split() for line in result.stdout.splitlines() if line.startswith("[GNUPG:] VALIDSIG ")]
    if result.returncode or len(signatures) != 1 or FINGERPRINT not in (signatures[0][2], signatures[0][-1]):
        raise ValueError("release_manifest_signature_failed")
    manifest = json.loads((root / "manifest.json").read_text())
    if manifest.get("version") != "2.1.258":
        raise ValueError("release_manifest_version_mismatch")
    for spec in lock["artifacts"]:
        if spec["name"] == "claude":
            platform = "linux-x64" if spec["platform"] == "linux/amd64" else "linux-arm64"
            if (manifest["platforms"][platform]["checksum"] != spec["sha256"]
                    or manifest["platforms"][platform]["size"] != spec["size"]):
                raise ValueError("signed_binary_hash_mismatch")
    lab.save("toolchain-provenance.json", {"metadata": records, "claude_validsig": FINGERPRINT,
        "bun_pgp_verified": False, "binary_execution": False})
    results = []
    # Native amd64 only: arm64 is metadata-locked, not downloaded or executed.
    for spec in lock["artifacts"]:
        if spec["platform"] != "linux/amd64":
            continue
        lab.capacity()
        records[spec["file"]] = fetch(spec["source_url"], root / spec["file"], spec["size"],
            expected_size=spec["size"], expected_sha=spec["sha256"])
        filename = "claude" if spec["name"] == "claude" else "bun-" + spec["version"]
        receipt = stage_binary(root / spec["file"], spec, binaries / filename)
        results.append({"file": filename, "artifact": spec, "staged": receipt})
        print(json.dumps({"downloaded": filename, "sha256": records[spec["file"]]["sha256"]}), flush=True)
    lab.save("toolchain-downloads.json", {"inputs": records, "binaries": results})
