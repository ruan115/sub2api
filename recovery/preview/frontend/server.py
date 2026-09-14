"""Serve reviewed original assets in an isolated, read-only loopback preview.

No network client, proxy, DB, environment configuration, production login, or
original-file writes. Quarantined bytes are never read by this module.
"""

from __future__ import annotations

import argparse
import hashlib
from html import escape
from html.parser import HTMLParser
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import re
import sys
import threading
from urllib.parse import unquote, urlsplit

from recovery.collectors.frontend import capture
from recovery.preview.frontend.fixtures import get_fixture
from recoverykit.evidence.errors import EvidenceError
from recoverykit.evidence.filesystem import (
    absolute_path, load_json_bytes, private_inventory, read_regular,
)


ROOT = Path(__file__).resolve().parents[2]
PIN = ROOT / "baselines/portunex/frontend/preservation-2026-09-14.json"
SOURCE_ORIGIN = "http://216.106.185.119:8080"
FONT_STYLESHEET = ("https://fonts.googleapis.com/css2?family=Inter:ital,opsz,wght@0,14..32,100..900;"
                   "1,14..32,100..900&family=JetBrains+Mono:wght@400;500;600&display=swap")
FONT_PRECONNECTS = ("https://fonts.googleapis.com", "https://fonts.gstatic.com")
ENTRY_MODULE = "assets/entry.client-COt7fs2Y.js"
QUARANTINED_PROVIDER = "assets/_dashboard.providers-ABk0JmwU.js"
MIME = {
    ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
    ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml", ".ico": "image/x-icon",
    ".jpg": "image/jpeg", ".png": "image/png", ".avif": "image/avif",
}
SPA_PATHS = {"/", "/dashboard", "/auth", "/403"}


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def load_capture(capture_root, pin_path=PIN):
    """Pin metadata and every served byte; do not open quarantine contents.

    capture.verify was used during preservation. Calling it here would read
    quarantine, so this narrower verification deliberately has separate status.
    """
    root = absolute_path(capture_root, required=True)
    pin_path = absolute_path(pin_path, required=True)
    pin = load_json_bytes(read_regular(pin_path.parent, pin_path.name, capture.MAX_METADATA))
    inventory_raw = read_regular(root, "inventory.json", capture.MAX_METADATA)
    receipt_raw = read_regular(root, "receipt.json", capture.MAX_METADATA)
    inventory = capture.validate_inventory(load_json_bytes(inventory_raw))
    receipt = load_json_bytes(receipt_raw)
    if (type(pin) is not dict or type(receipt) is not dict
            or pin.get("kind") != "ccmax.legacy_frontend.static_preservation"
            or pin.get("source") != capture.SOURCE
            or digest(inventory_raw) != pin.get("inventory_sha256")
            or digest(receipt_raw) != pin.get("receipt_sha256")
            or receipt.get("mode") != "capture"
            or receipt.get("collector_sha256") != pin.get("collector_sha256")
            or receipt.get("inventory_sha256") != pin.get("inventory_sha256")):
        raise EvidenceError("preview metadata pin mismatch")
    records = receipt.get("records")
    entries = pin.get("entries")
    if (type(records) is not list or type(entries) is not list
            or any(type(entry) is not dict for entry in entries)
            or any(type(record) is not dict for record in records)
            or len(entries) != len(inventory["entries"]) or len(records) != len(entries)
            or len(entries) != pin.get("file_count")
            or sum(entry["bytes"] for entry in inventory["entries"]) != pin.get("total_bytes")):
        raise EvidenceError("preview inventory mismatch")
    assets, quarantine = {}, set()
    expected_files = {"inventory.json", "receipt.json"}
    for entry, source, record in zip(entries, inventory["entries"], records):
        category = entry.get("category")
        name = source["path"]
        if (category not in {"files", "reference", "quarantine"}
                or entry != dict(source, category=category, reason=record.get("reason"))
                or record != {"path": name, "stored_path": category + "/" + name,
                              "category": category, "reason": entry.get("reason")}):
            raise EvidenceError("preview record mismatch")
        expected_files.add(record["stored_path"])
        if category == "quarantine":
            quarantine.add(name)
            continue
        raw = read_regular(root, record["stored_path"], capture.MAX_FILE)
        if (len(raw) != source["bytes"] or digest(raw) != source["sha256"]
                or capture.classify(name, raw) != (category, entry["reason"])):
            raise EvidenceError("preview served content mismatch")
        assets[name] = raw
    # Enumerates names, permissions and file types, never file content.
    if private_inventory(root) != expected_files or "index.html" not in assets:
        raise EvidenceError("preview file set mismatch")
    return assets, quarantine


class LinkParser(HTMLParser):
    external = False
    href = ""

    def handle_starttag(self, tag, attrs):
        if tag.lower() != "link":
            return
        self.href = dict(attrs).get("href", "")
        parsed = urlsplit(self.href)
        self.external = bool(parsed.scheme or parsed.netloc or self.href.startswith("//"))


def prepare_html(raw, local_origin=None):
    value = raw.decode("utf-8")

    def keep_local_link(match):
        parser = LinkParser()
        parser.feed(match.group())
        return "" if parser.external and parser.href != local_origin else match.group()

    value = re.sub(r"<link\b[^>]*>", keep_local_link, value, flags=re.I)
    value = re.sub(r"<base\b[^>]*>", "", value, flags=re.I)
    if not re.search(r"<head\b[^>]*>", value, flags=re.I):
        raise EvidenceError("preview HTML head unavailable")
    return value.encode()


def rewrite_fonts(raw, local_origin):
    """Keep React's head links and original HTML consistent without font egress."""
    css_count, preconnect_count = 0, 0
    for source in (FONT_STYLESHEET.replace("&", "&amp;"), FONT_STYLESHEET):
        css_count += raw.count(source.encode())
        raw = raw.replace(source.encode(), b"/__preview__/fonts.css")
    for source in FONT_PRECONNECTS:
        preconnect_count += raw.count(source.encode())
        raw = raw.replace(source.encode(), local_origin.encode())
    return raw, css_count, preconnect_count


def origin(port):
    return "http://127.0.0.1:" + str(port)


def port_number(value):
    port = int(value)
    if not 1 <= port <= 65535:
        raise argparse.ArgumentTypeError("port must be between 1 and 65535")
    return port


def request_path(value):
    if len(value) > 4096 or not value.startswith("/") or value.startswith("//"):
        raise ValueError("invalid request path")
    parsed = urlsplit(value)
    path = unquote(parsed.path, errors="strict")
    if (parsed.scheme or parsed.netloc or parsed.fragment or "\\" in path or "%" in path
            or any(ord(char) < 32 or ord(char) == 127 for char in path)
            or any(part in {".", ".."} for part in path.split("/"))
            or path.startswith("//") or "//" in path):
        raise ValueError("invalid request path")
    return path


class Preview:
    def __init__(self, assets, quarantine, wrapper_port=18091, legacy_port=18092, provider_preview=None):
        if wrapper_port == legacy_port and wrapper_port != 0:
            raise ValueError("preview ports must differ")
        self.assets = dict(assets)
        self.quarantine = frozenset(quarantine)
        self.loaded_original_files = len(self.assets)
        self.provider_preview = provider_preview is not None
        if provider_preview is not None:
            if (type(provider_preview) is not bytes or QUARANTINED_PROVIDER not in self.quarantine
                    or QUARANTINED_PROVIDER in self.assets):
                raise ValueError("invalid provider preview")
            self.assets[QUARANTINED_PROVIDER] = provider_preview
        self.guard = read_regular(Path(__file__).resolve().parent, "guard.js", 65536)
        prepare_html(self.assets["index.html"])  # Validate before binding, without changing source bytes.
        self.wrapper = ThreadingHTTPServer(("127.0.0.1", wrapper_port), self.handler("wrapper"))
        try:
            self.legacy = ThreadingHTTPServer(("127.0.0.1", legacy_port), self.handler("legacy"))
        except BaseException:
            self.wrapper.server_close()
            raise
        self.wrapper_origin = origin(self.wrapper.server_port)
        self.legacy_origin = origin(self.legacy.server_port)
        self.replacements = []
        for name, raw in list(self.assets.items()):
            changed = raw
            api_count, css_count, preconnect_count = 0, 0, 0
            if name.endswith(".js"):
                api_count = raw.count(SOURCE_ORIGIN.encode())
                changed = raw.replace(SOURCE_ORIGIN.encode(), self.legacy_origin.encode())
            if name == "index.html" or name == "assets/root-CFGZHjRn.js":
                changed, css_count, preconnect_count = rewrite_fonts(changed, self.legacy_origin)
            if name == "index.html":
                changed = prepare_html(changed, self.legacy_origin)
            if name == ENTRY_MODULE:
                changed = self.guard + b"\n" + changed
            if changed != raw:
                self.assets[name] = changed
                self.replacements.append({"path": name, "source_sha256": digest(raw),
                                          "preview_sha256": digest(changed),
                                          "exact_url_replacements": api_count,
                                          "font_stylesheet_replacements": css_count,
                                          "font_preconnect_replacements": preconnect_count,
                                          "entry_guard_prepends": int(name == ENTRY_MODULE)})
        self.threads = []

    def csp(self, kind):
        base = ("default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "
                "connect-src 'self'; img-src 'self' data: blob:; font-src 'self'; "
                "object-src 'none'; base-uri 'none'; form-action 'none'; worker-src 'none'; ")
        if kind == "wrapper":
            return base + "frame-src " + self.legacy_origin + "; frame-ancestors 'none'"
        return (base + "frame-src 'none'; frame-ancestors " + self.wrapper_origin
                + "; sandbox allow-scripts allow-same-origin")

    def wrapper_html(self):
        endpoint = escape(self.legacy_origin, quote=True)
        provider_label = "账号页（隔离派生预览）" if self.provider_preview else "账号页（待审核）"
        return f'''<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>DIDI · 本地隔离预览</title>
<style>html,body{{margin:0;height:100%;font:14px system-ui;background:#101216;color:#fff}}
body{{display:flex;flex-direction:column}}header{{flex:none;padding:12px 18px;border-bottom:1px solid #41434a;
display:flex;gap:16px;align-items:center;flex-wrap:wrap}}strong{{color:#ffc976}}nav{{display:flex;gap:8px}}
button{{padding:7px 12px;border:1px solid #565961;border-radius:7px;color:#fff;background:#272a30;cursor:pointer}}
small{{color:#bfc3cc}}iframe{{flex:1;width:100%;border:0;background:#fff}}</style></head><body>
<header><strong>本地隔离预览 · 演示数据 · 写操作禁用</strong><nav>
<button data-path="/">原版首页</button><button data-path="/dashboard">管理总览</button>
<button data-path="/dashboard/providers">{provider_label}</button></nav>
<small>旧发布资源；不是已恢复的可维护源工程。字体使用本地回退。</small></header>
<iframe title="旧版 DIDI 页面预览" sandbox="allow-scripts allow-same-origin" referrerpolicy="no-referrer"
src="{endpoint}/"></iframe><script>const frame=document.querySelector('iframe');
document.querySelectorAll('button[data-path]').forEach(b=>b.addEventListener('click',()=>{{
frame.src={json.dumps(self.legacy_origin)}+b.dataset.path;}}));</script></body></html>'''.encode()

    def handler(self, kind):
        preview = self

        class Handler(BaseHTTPRequestHandler):
            server_version = "LocalReadOnlyPreview"
            sys_version = ""

            def log_message(self, *_):
                pass

            def parse_request(self):
                if not super().parse_request():
                    return False
                parts = self.raw_requestline.split()
                # The stdlib normalizes a leading // before dispatch; reject
                # its raw form too instead of accidentally accepting an alias.
                if len(parts) >= 2 and parts[1].startswith(b"//"):
                    self.reject(400, "preview_path_rejected")
                    return False
                return True

            def security_ok(self):
                expected = preview.wrapper_origin if kind == "wrapper" else preview.legacy_origin
                hosts = self.headers.get_all("Host", [])
                origins = self.headers.get_all("Origin", [])
                fetch_sites = self.headers.get_all("Sec-Fetch-Site", [])
                if hosts != [expected.removeprefix("http://")]:
                    return False
                allowed_origins = {expected}
                if kind == "legacy":
                    allowed_origins.add(preview.wrapper_origin)
                same_site_navigation = (kind == "legacy"
                                        and self.headers.get("Sec-Fetch-Mode") == "navigate"
                                        and self.headers.get("Sec-Fetch-Dest") == "iframe")
                return (len(origins) <= 1 and (not origins or origins[0] in allowed_origins)
                        and len(fetch_sites) <= 1
                        and (not fetch_sites or fetch_sites[0] in {"none", "same-origin"}
                             or (fetch_sites[0] == "same-site" and same_site_navigation)))

            def respond(self, status, body, content_type="application/json; charset=utf-8"):
                self.close_connection = True
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Content-Security-Policy", preview.csp(kind))
                self.send_header("Cache-Control", "no-store")
                self.send_header("X-Content-Type-Options", "nosniff")
                self.send_header("Cross-Origin-Resource-Policy", "same-origin")
                self.send_header("Referrer-Policy", "no-referrer")
                self.send_header("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
                self.send_header("Connection", "close")
                self.end_headers()
                if self.command != "HEAD":
                    self.wfile.write(body)

            def reject(self, status, error):
                self.respond(status, json.dumps({"error": error, "preview": True}).encode())

            def do_GET(self):
                if not self.security_ok():
                    return self.reject(403, "preview_origin_rejected")
                try:
                    path = request_path(self.path)
                except (ValueError, UnicodeError):
                    return self.reject(400, "preview_path_rejected")
                if path == "/__preview__/status":
                    report = {"mode": "local_visual_preview", "synthetic_data": True,
                              "writes_enabled": False, "production_connected": False,
                              "loaded_original_files": preview.loaded_original_files,
                              "loaded_derived_files": int(preview.provider_preview),
                              "quarantined_files_not_read": len(preview.quarantine),
                              "quarantine_placeholder_modules": int(QUARANTINED_PROVIDER in preview.quarantine
                                                                    and not preview.provider_preview),
                              "asset_transforms": preview.replacements}
                    return self.respond(200, json.dumps(report).encode())
                if kind == "wrapper":
                    if path == "/":
                        return self.respond(200, preview.wrapper_html(), MIME[".html"])
                    return self.reject(404, "preview_not_found")
                if path == "/__preview__/guard.js":
                    return self.respond(200, preview.guard, MIME[".js"])
                if path == "/__preview__/fonts.css":
                    return self.respond(200, b"/* Local preview: external fonts disabled; use system fallbacks. */\n",
                                        MIME[".css"])
                if path.startswith("/portunex/"):
                    fixture = get_fixture(path)
                    if fixture is None:
                        return self.reject(503, "preview_api_not_implemented")
                    return self.respond(200, json.dumps(fixture, ensure_ascii=False).encode())
                name = path.lstrip("/")
                if name in preview.quarantine:
                    if name == QUARANTINED_PROVIDER:
                        if preview.provider_preview:
                            return self.respond(200, preview.assets[name], MIME[".js"])
                        stub = ("export default function QuarantinedAccountPage(){return "
                                + json.dumps("账号页面仍在安全审核：本地预览没有加载被隔离的原 JS。此页为占位，不代表原版外观。", ensure_ascii=False)
                                + ";}")
                        return self.respond(200, stub.encode(), MIME[".js"])
                    return self.reject(403, "preview_asset_quarantined")
                if path in SPA_PATHS or path.startswith("/dashboard/"):
                    name = "index.html"
                if name not in preview.assets:
                    return self.reject(404, "preview_not_found")
                return self.respond(200, preview.assets[name], MIME[Path(name).suffix])

            do_HEAD = do_GET

            def deny_write(self):
                if not self.security_ok():
                    return self.reject(403, "preview_origin_rejected")
                return self.reject(405, "preview_write_disabled")

            do_POST = do_PUT = do_PATCH = do_DELETE = do_OPTIONS = do_TRACE = do_CONNECT = deny_write

        return Handler

    def start(self):
        for server in (self.wrapper, self.legacy):
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            self.threads.append(thread)

    def close(self):
        for server in (self.wrapper, self.legacy):
            if self.threads:
                server.shutdown()
            server.server_close()
        for thread in self.threads:
            thread.join(timeout=2)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--capture-root", required=True)
    parser.add_argument("--provider-preview-root", help="optional, explicitly reviewed derived provider directory")
    parser.add_argument("--wrapper-port", type=port_number, default=18091)
    parser.add_argument("--legacy-port", type=port_number, default=18092)
    args = parser.parse_args()
    preview = None
    try:
        assets, quarantine = load_capture(args.capture_root)
        provider_preview = None
        if args.provider_preview_root is not None:
            from recovery.preview.frontend.prepare_provider import load_prepared
            provider_preview = load_prepared(args.provider_preview_root)
        preview = Preview(assets, quarantine, args.wrapper_port, args.legacy_port, provider_preview)
        preview.start()
        print("Local preview ready: " + preview.wrapper_origin + "/", flush=True)
        print("Isolated assets: " + preview.legacy_origin + "/", flush=True)
        threading.Event().wait()
    except KeyboardInterrupt:
        return 0
    except (EvidenceError, OSError, ValueError, TypeError, KeyError):
        print("Local preview failed; no source content or private paths printed", file=sys.stderr)
        return 1
    finally:
        if preview is not None:
            preview.close()


if __name__ == "__main__":
    sys.exit(main())
