"""Local synthetic assets and loopback HTTP only; no original JS execution."""

import base64
import copy
import http.client
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from recovery.collectors.frontend import capture
from recovery.preview.frontend import server
from recovery.preview.frontend.fixtures import get_fixture
from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import json_bytes


HTML = (b'<!doctype html><html><head><link href="https://fonts.example.invalid/style" rel="stylesheet">'
        b'<link href="//fonts.example.invalid" rel="preconnect"><link href="/assets/site.css" rel="stylesheet">'
        b'<base href="https://example.invalid/"><script>window.fixture="preserved";</script></head>'
        b'<body><div id="root"></div><script type="module" src="/assets/app.js"></script></body></html>')
ASSETS = {"index.html": HTML, "assets/app.js": b'const endpoint="http://216.106.185.119:8080";',
          "assets/site.css": b"body{color:red}", server.ENTRY_MODULE: b"/* synthetic entry; never executed */"}


class LoaderTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(dir=Path(tempfile.gettempdir()).resolve())
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.capture_root = self.root / "capture"
        self.pin_path = self.root / "pin.json"
        files = dict(ASSETS)
        files[server.QUARANTINED_PROVIDER] = b'https://' + b'synthetic:synthetic' + b'@example.invalid/'
        envelope = {
            "inventory": {"schema_version": 1, "kind": "portunex.frontend.inventory", "source": capture.SOURCE,
                          "entries": [{"path": name, "bytes": len(raw), "sha256": capture.digest(raw)}
                                      for name, raw in sorted(files.items())]},
            "contents": {name: base64.b64encode(raw).decode() for name, raw in files.items()},
        }
        capture.preserve(self.capture_root, "capture", envelope)
        inventory_raw = (self.capture_root / "inventory.json").read_bytes()
        receipt_raw = (self.capture_root / "receipt.json").read_bytes()
        receipt = json.loads(receipt_raw)
        self.pin = {
            "kind": "ccmax.legacy_frontend.static_preservation", "source": capture.SOURCE,
            "inventory_sha256": capture.digest(inventory_raw), "receipt_sha256": capture.digest(receipt_raw),
            "collector_sha256": receipt["collector_sha256"], "file_count": len(files),
            "total_bytes": sum(len(raw) for raw in files.values()),
            "entries": [dict(entry, category=record["category"], reason=record["reason"])
                        for entry, record in zip(envelope["inventory"]["entries"], receipt["records"])],
        }
        self.pin_path.write_bytes(json_bytes(self.pin))

    def load(self):
        return server.load_capture(self.capture_root, self.pin_path)

    def test_loader_pins_and_never_reads_quarantined_bytes(self):
        read = server.read_regular
        paths = []

        def checked(root, name, limit):
            self.assertFalse(name.startswith("quarantine/"))
            paths.append(name)
            return read(root, name, limit)

        with patch.object(server, "read_regular", side_effect=checked):
            assets, quarantine = self.load()
        self.assertEqual(assets, ASSETS)
        self.assertEqual(quarantine, {server.QUARANTINED_PROVIDER})
        self.assertIn("files/index.html", paths)

    def test_metadata_hash_mismatch_rejected(self):
        for name in ("inventory.json", "receipt.json"):
            path = self.capture_root / name
            original = path.read_bytes()
            path.write_bytes(original + b" ")
            with self.assertRaises(EvidenceError):
                self.load()
            path.write_bytes(original)

    def test_file_tamper_rejected(self):
        (self.capture_root / "files/assets/app.js").write_bytes(b"changed")
        with self.assertRaises(EvidenceError):
            self.load()

    def test_category_pin_mismatch_rejected(self):
        pin = copy.deepcopy(self.pin)
        pin["entries"][0]["category"] = "reference"
        self.pin_path.write_bytes(json_bytes(pin))
        with self.assertRaises(EvidenceError):
            self.load()

    def test_unknown_extra_private_file_rejected(self):
        path = self.capture_root / "extra.txt"
        path.write_bytes(b"synthetic")
        path.chmod(0o600)
        with self.assertRaises(EvidenceError):
            self.load()

    def test_relative_capture_path_rejected(self):
        with self.assertRaises(EvidenceError):
            server.load_capture("relative", self.pin_path)

    def test_source_symlink_rejected(self):
        target = self.capture_root / "files/assets/app.js"
        target.unlink()
        target.symlink_to(self.pin_path)
        with self.assertRaises(EvidenceError):
            self.load()


class TransformTests(unittest.TestCase):
    def test_html_preserves_original_inline_script_and_local_css(self):
        html = server.prepare_html(HTML)
        self.assertNotIn(b"fonts.example.invalid", html)
        self.assertNotIn(b"<base", html)
        self.assertIn(b'window.fixture="preserved";', html)
        self.assertIn(b'href="/assets/site.css"', html)
        self.assertNotIn(b"/__preview__/guard.js", html)

    def test_missing_head_fails_closed(self):
        with self.assertRaises(EvidenceError):
            server.prepare_html(b"<html><body>fixture</body></html>")

    def test_encoded_external_link_removed(self):
        html = server.prepare_html(b'<html><head><link href="&#104;ttps://example.invalid/x"></head></html>')
        self.assertNotIn(b"example.invalid", html)

    def test_font_links_match_between_html_and_react_module(self):
        font_css_html = server.FONT_STYLESHEET.replace("&", "&amp;")
        html = ('<html><head><link rel="preconnect" href="https://fonts.googleapis.com">'
                '<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin="anonymous">'
                f'<link rel="stylesheet" href="{font_css_html}"></head></html>').encode()
        module = json.dumps([*server.FONT_PRECONNECTS, server.FONT_STYLESHEET]).encode()
        expected_origin = "http://127.0.0.1:18092"
        html, css_count, connect_count = server.rewrite_fonts(html, expected_origin)
        self.assertEqual((css_count, connect_count), (1, 2))
        html = server.prepare_html(html, expected_origin)
        module, css_count, connect_count = server.rewrite_fonts(module, expected_origin)
        self.assertEqual((css_count, connect_count), (1, 2))
        for transformed in (html, module):
            self.assertNotIn(b"fonts.googleapis.com", transformed)
            self.assertNotIn(b"fonts.gstatic.com", transformed)
            self.assertIn(b"/__preview__/fonts.css", transformed)
            self.assertEqual(transformed.count(expected_origin.encode()), 2)
        self.assertIn(b'crossorigin="anonymous"', html)

    def test_paths_reject_traversal_absolute_and_ambiguity(self):
        for path in ("/../index.html", "/%2e%2e/index.html", "/%252e%252e/a", "//evil.invalid/",
                     "http://evil.invalid/", "/assets\\app.js", "/%00", "/a//b", "/a#b", "/%ff"):
            with self.subTest(path=path), self.assertRaises((ValueError, UnicodeError)):
                server.request_path(path)
        self.assertEqual(server.request_path("/assets/app.js?cache=123"), "/assets/app.js")

    def test_fixture_is_copy_and_explicit(self):
        original = get_fixture("/portunex/users/me")
        self.assertEqual(original["email"], "preview@example.invalid")
        original["role"] = "changed"
        self.assertEqual(get_fixture("/portunex/users/me")["role"], "admin")
        self.assertIsNone(get_fixture("/portunex/users"))
        self.assertEqual(get_fixture("/portunex/admin/stats")["time_series"], [])

    def test_provider_fixture_is_synthetic_without_credentials(self):
        fixture = get_fixture("/portunex/admin/providers")
        self.assertEqual(fixture["total"], 1)
        account = fixture["providers"][0]
        self.assertIn("合成账号", account["name"])
        self.assertEqual(account["kind"], "claude_code")
        self.assertEqual(account["max_concurrent"], 2)
        for key in ("base_url", "http_proxy", "socks5_proxy"):
            self.assertIsNone(account[key])
        for key in account:
            self.assertFalse(any(word in key.lower() for word in ("token", "secret", "password", "credential")))
        account["window_states"].append("mutation")
        self.assertEqual(get_fixture("/portunex/admin/providers")["providers"][0]["window_states"], [])
        errors = get_fixture("/portunex/admin/providers/error-types")
        self.assertEqual(errors, {"types": [], "total": 1, "with_error": 0, "without_error": 1})
        windows = get_fixture("/portunex/admin/providers/window-summary")
        self.assertEqual(windows["groups"], [])
        self.assertEqual(windows["healthy_provider_count"], 1)
        self.assertEqual(windows["total_max_concurrent"], 2)
        self.assertEqual(windows["total_active_concurrent"], 0)

    def test_guard_preserves_theme_and_disables_summary_polling(self):
        guard = (server.ROOT / "preview/frontend/guard.js").read_text()
        self.assertNotIn("localStorage.clear", guard)
        self.assertNotIn("sessionStorage.clear", guard)
        self.assertNotIn('setItem("theme"', guard)
        self.assertIn('setItem("portunex_window_summary_refresh_interval", "0")', guard)
        self.assertIn('setItem("portunex_token", "sess_LOCAL_PREVIEW_SYNTHETIC_NOT_A_CREDENTIAL")', guard)


class HTTPTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.preview = server.Preview(ASSETS, {server.QUARANTINED_PROVIDER, "assets/blocked.js"}, 0, 0)
        cls.preview.start()

    @classmethod
    def tearDownClass(cls):
        cls.preview.close()

    def request(self, path, method="GET", headers=None, body=None, wrapper=False):
        port = self.preview.wrapper.server_port if wrapper else self.preview.legacy.server_port
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
        try:
            connection.request(method, path, body=body, headers=headers or {})
            response = connection.getresponse()
            return response.status, dict(response.getheaders()), response.read()
        finally:
            connection.close()

    def test_wrapper_permanent_label_separate_origin_and_sandbox(self):
        status, headers, body = self.request("/", wrapper=True)
        self.assertEqual(status, 200)
        self.assertIn("本地隔离预览 · 演示数据 · 写操作禁用".encode(), body)
        self.assertIn(b'sandbox="allow-scripts allow-same-origin"', body)
        self.assertIn(self.preview.legacy_origin.encode(), body)
        self.assertNotEqual(self.preview.wrapper_origin, self.preview.legacy_origin)
        self.assertIn("frame-src " + self.preview.legacy_origin, headers["Content-Security-Policy"])

    def test_legacy_csp_limits_execution_and_no_cache(self):
        status, headers, _ = self.request("/dashboard")
        self.assertEqual(status, 200)
        csp = headers["Content-Security-Policy"]
        for directive in ("connect-src 'self'", "frame-src 'none'", "worker-src 'none'", "object-src 'none'",
                          "form-action 'none'", "sandbox allow-scripts allow-same-origin",
                          "frame-ancestors " + self.preview.wrapper_origin):
            self.assertIn(directive, csp)
        self.assertNotIn("unsafe-eval", csp)
        self.assertEqual(headers["Cache-Control"], "no-store")
        self.assertEqual(headers["X-Content-Type-Options"], "nosniff")
        self.assertEqual(headers["Referrer-Policy"], "no-referrer")

    def test_host_origin_and_fetch_site_rejected(self):
        for headers in ({"Host": "evil.invalid"}, {"Host": "localhost"}, {"Origin": "https://evil.invalid"},
                        {"Origin": "null"}, {"Sec-Fetch-Site": "cross-site"}, {"Sec-Fetch-Site": "same-site"}):
            self.assertEqual(self.request("/", headers=headers)[0], 403)

    def test_same_site_iframe_navigation_allowed(self):
        self.assertEqual(self.request("/", headers={"Sec-Fetch-Site": "same-site",
                                                    "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"})[0], 200)

    def test_writes_and_options_disabled(self):
        for method in ("POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE", "CONNECT"):
            status, headers, body = self.request("/portunex/users/me", method, body=b"must-not-be-read")
            self.assertEqual(status, 405)
            self.assertEqual(headers["Connection"], "close")
            self.assertNotIn(b"must-not-be-read", body)

    def test_traversal_no_listing_or_private_metadata(self):
        for path, expected in (("/%2e%2e/inventory.json", 400), ("/inventory.json", 404),
                               ("//dashboard", 400),
                               ("/receipt.json", 404), ("/assets/", 404), ("/files/index.html", 404),
                               ("/quarantine/" + server.QUARANTINED_PROVIDER, 404)):
            status, _, body = self.request(path)
            self.assertEqual(status, expected)
            self.assertNotIn(b"root", body)

    def test_known_spa_only(self):
        self.assertEqual(self.request("/dashboard/providers")[0], 200)
        self.assertEqual(self.request("/unknown")[0], 404)
        self.assertEqual(self.request("/auth/oauth/callback")[0], 404)

    def test_production_literal_remapped_in_memory_and_provenance_only(self):
        status, _, body = self.request("/assets/app.js?v=1")
        self.assertEqual(status, 200)
        self.assertNotIn(server.SOURCE_ORIGIN.encode(), body)
        self.assertIn(self.preview.legacy_origin.encode(), body)
        self.assertIn(server.SOURCE_ORIGIN.encode(), ASSETS["assets/app.js"])
        status, _, body = self.request("/__preview__/status")
        self.assertEqual(status, 200)
        report = json.loads(body)
        app_transform = next(item for item in report["asset_transforms"] if item["path"] == "assets/app.js")
        self.assertEqual(app_transform["exact_url_replacements"], 1)
        self.assertNotIn("const endpoint", body.decode())
        self.assertFalse(report["production_connected"])

    def test_guard_prepends_only_entry_and_does_not_change_head_tree(self):
        status, _, body = self.request("/" + server.ENTRY_MODULE)
        self.assertEqual(status, 200)
        self.assertEqual(body, self.preview.guard + b"\n" + ASSETS[server.ENTRY_MODULE])
        status, _, html = self.request("/")
        self.assertEqual(status, 200)
        self.assertNotIn(b"/__preview__/guard.js", html)
        self.assertIn(b'window.fixture="preserved";', html)
        _, _, status_body = self.request("/__preview__/status")
        records = json.loads(status_body)["asset_transforms"]
        entry_transform = next(record for record in records if record["path"] == server.ENTRY_MODULE)
        self.assertEqual(entry_transform["entry_guard_prepends"], 1)
        self.assertTrue(all(record["entry_guard_prepends"] == 0 for record in records
                            if record["path"] != server.ENTRY_MODULE))

    def test_quarantine_replacement_and_default_deny(self):
        status, _, body = self.request("/" + server.QUARANTINED_PROVIDER)
        self.assertEqual(status, 200)
        self.assertIn(b"export default function", body)
        self.assertIn("此页为占位".encode(), body)
        self.assertEqual(self.request("/assets/blocked.js")[0], 403)

    def test_mock_me_stats_and_unknown_api(self):
        status, _, body = self.request("/portunex/users/me")
        self.assertEqual(status, 200)
        self.assertEqual(json.loads(body)["id"], "preview-user")
        self.assertNotIn("token", json.loads(body))
        for path in ("/portunex/users/me/stats", "/portunex/admin/stats"):
            status, _, body = self.request(path)
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body)["total_requests"], 128)
        self.assertEqual(self.request("/portunex/providers")[0], 503)

    def test_mock_provider_endpoints(self):
        for path in ("/portunex/admin/providers", "/portunex/admin/providers/error-types",
                     "/portunex/admin/providers/window-summary"):
            status, _, body = self.request(path + "?page=1")
            self.assertEqual(status, 200)
            self.assertEqual(json.loads(body), get_fixture(path))
        self.assertEqual(self.request("/portunex/admin/providers/1")[0], 503)
        self.assertEqual(self.request("/portunex/admin/providers", method="POST", body=b"{}")[0], 405)

    def test_local_font_stylesheet_is_noop(self):
        status, headers, body = self.request("/__preview__/fonts.css")
        self.assertEqual(status, 200)
        self.assertEqual(headers["Content-Type"], server.MIME[".css"])
        self.assertNotIn(b"@import", body)
        self.assertNotIn(b"url(", body)
        self.assertIn(b"system fallbacks", body)

    def test_head_has_no_body_and_no_body_logging(self):
        status, headers, body = self.request("/assets/app.js", method="HEAD")
        self.assertEqual(status, 200)
        self.assertEqual(body, b"")
        self.assertGreater(int(headers["Content-Length"]), 0)


class DerivedProviderTests(unittest.TestCase):
    def test_explicit_derived_bytes_do_not_replace_quarantine_evidence(self):
        derived = b'export default function SyntheticDerived(){return "synthetic derived fixture";}'
        preview = server.Preview(ASSETS, {server.QUARANTINED_PROVIDER}, 0, 0, derived)
        self.addCleanup(preview.close)
        preview.start()
        self.assertNotIn(server.QUARANTINED_PROVIDER, ASSETS)
        self.assertIn(server.QUARANTINED_PROVIDER, preview.quarantine)
        self.assertIn("账号页（隔离派生预览）".encode(), preview.wrapper_html())
        connection = http.client.HTTPConnection("127.0.0.1", preview.legacy.server_port, timeout=3)
        self.addCleanup(connection.close)
        connection.request("GET", "/" + server.QUARANTINED_PROVIDER)
        response = connection.getresponse()
        self.assertEqual(response.status, 200)
        self.assertEqual(response.read(), derived)
        connection.close()
        connection.request("GET", "/__preview__/status")
        response = connection.getresponse()
        report = json.loads(response.read())
        self.assertEqual(report["loaded_original_files"], 4)
        self.assertEqual(report["loaded_derived_files"], 1)
        self.assertEqual(report["quarantined_files_not_read"], 1)
        self.assertEqual(report["quarantine_placeholder_modules"], 0)

    def test_derived_cannot_override_ordinary_original_asset(self):
        with self.assertRaises(ValueError):
            server.Preview(ASSETS, set(), 0, 0, b"fixture")


if __name__ == "__main__":
    unittest.main()
