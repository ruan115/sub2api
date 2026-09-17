import hashlib
import io
from pathlib import Path
import tempfile
import time
import signal
import unittest
from unittest.mock import Mock, patch
import urllib.request
from urllib.parse import urlunsplit
from lab.acquire import HTTPSRedirect, fetch, download_deadline


class PublicAcquisitionTests(unittest.TestCase):
    def test_deadline_interrupts_a_blocked_call_and_restores_handler(self):
        before = signal.getsignal(signal.SIGALRM)
        with self.assertRaises(TimeoutError):
            with download_deadline(0.02):
                time.sleep(0.2)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))
        self.assertEqual(signal.getsignal(signal.SIGALRM), before)

    def test_late_eof_cannot_return_success(self):
        with tempfile.TemporaryDirectory() as tmp:
            opener = Mock()
            opener.open.return_value = io.BytesIO(b"")
            with patch("lab.acquire.urllib.request.build_opener", return_value=opener), \
                    patch("lab.acquire.time.monotonic", side_effect=[0, 1, 301]), self.assertRaises(TimeoutError):
                fetch("https://downloads.claude.ai/synthetic", Path(tmp) / "artifact", 1)

    def test_public_download_hash_size_and_no_proxy(self):
        with tempfile.TemporaryDirectory() as tmp:
            opener = Mock()
            opener.open.return_value = io.BytesIO(b"synthetic")
            with patch("lab.acquire.urllib.request.build_opener", return_value=opener) as factory:
                result = fetch("https://downloads.claude.ai/synthetic", Path(tmp) / "artifact", 9,
                               expected_size=9, expected_sha=hashlib.sha256(b"synthetic").hexdigest())
            self.assertEqual(factory.call_args.args[0].proxies, {})
            self.assertEqual(result["size"], 9)
            self.assertEqual((Path(tmp) / "artifact").stat().st_mode & 0o777, 0o600)

    def test_mismatch_oversize_and_existing_file_fail(self):
        for data, size, digest in ((b"too long!", 1, None), (b"a", 2, None), (b"a", 1, "0" * 64)):
            with self.subTest(data=data, size=size), tempfile.TemporaryDirectory() as tmp:
                opener = Mock()
                opener.open.return_value = io.BytesIO(data)
                with patch("lab.acquire.urllib.request.build_opener", return_value=opener), self.assertRaises(ValueError):
                    fetch("https://downloads.claude.ai/synthetic", Path(tmp) / "artifact", size,
                          expected_size=size, expected_sha=digest)

    def test_redirect_cannot_downgrade_or_leave_official_download_hosts(self):
        handler = HTTPSRedirect()
        request = urllib.request.Request("https://github.com/synthetic")
        synthetic_userinfo = urlunsplit(("https", "synthetic" + ":" + "synthetic@github.com", "/x", "", ""))
        for url in ("http://github.com/x", "https://evil.invalid/x", synthetic_userinfo,
                    "https://github.com:444/x", "file:///tmp/synthetic"):
            with self.subTest(url=url), self.assertRaises(ValueError):
                handler.redirect_request(request, None, 302, "", {}, url)
        result = handler.redirect_request(request, None, 302, "", {},
                    "https://release-assets.githubusercontent.com/synthetic")
        self.assertEqual(result.host, "release-assets.githubusercontent.com")
