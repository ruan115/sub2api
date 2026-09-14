# Original frontend: isolated local visual preview

This is a **read-only visual preview**, not the recovered TSX project, a login
implementation, an API compatibility claim, or a production deployment.
Fixtures contain only obviously synthetic data. No DB, environment credentials,
network client, proxy, SSH, or live API is used by the server.

From the repository root, with an explicit absolute private capture directory:

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling:. python3 -m recovery.preview.frontend.server \
  --capture-root /absolute/private/capture
```

Open `http://127.0.0.1:18091/`; Ctrl-C stops both servers. Optional
`--wrapper-port` and `--legacy-port` accept distinct ports 1–65535. The bind
address is always `127.0.0.1`, never configurable. A port collision fails without
stopping or reconfiguring the existing listener.

`browser-guard.js` and `playwright-cli.json` are for a fresh dedicated Playwright
CLI session using **only the default ports 18091/18092**. The browser guard does
not follow custom server ports; do not reuse it unchanged with port overrides.

## Modules and boundary

- `server.py`: metadata pin validation, in-memory asset transformation, local
  servers and fixed synthetic API allowlist. No raw private-directory serving.
- `fixtures.py`: fictional user and statistics; no true credentials or users.
- `prepare_provider.py`: optional pinned placeholder-only scrub into a new
  private directory, preserving the quarantined original. Prepare it explicitly:

  ```sh
  PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling:. python3 -m recovery.preview.frontend.prepare_provider \
    --capture-root /absolute/private/capture --destination /absolute/private/new-derived
  ```

- `guard.js`: runs before the original entry's hydration; seeds a synthetic local token and
  blocks write/network/pop-up/form actions. It overwrites only the preview token
  and sets the window-summary refresh interval to `0`, preserving theme choices
  and unrelated storage; do not reuse this port for another application.

An optional `--provider-preview-root /absolute/private/derived` loads only the
independently reviewed, pinned derivative through `prepare_provider.load_prepared`.
It does not read or promote the quarantined original. Without that explicit
option the Provider page remains a placeholder. The status endpoint separately
reports original/derived/quarantined/placeholder counts. The derived view still
has only synthetic API fixtures and all writes disabled.

The top-level wrapper uses a different origin from the legacy iframe, retains
the warning banner, and restricts frame navigation to the legacy loopback
origin. The iframe sandbox forbids forms, popups and top navigation. Both server
CSPs deny remote resources; the legacy document also carries CSP sandbox.
Host/Origin/Fetch-Metadata checks reject unrelated callers, and no CORS grants
are issued. Browser-level routing is recommended as an additional boundary
when executing recovered code, not as proof the original code is trustworthy.

Repository `preservation-2026-09-14.json` pins receipt and inventory hashes and
per-file bytes/category. Startup reads only approved `files/` and `reference/`
entries. `capture.verify` was used at preservation time; the preview deliberately
does **not** call it, because it would read quarantine. The preview verifies
every served original byte and inventories only quarantine names/permissions,
never its content. The known quarantined Provider chunk gets a newly authored,
plain-text placeholder component unless its separately pinned derivative was
explicitly selected; every other quarantined asset is rejected.

The original HTML and hydration scripts are retained. The three known Google
font URLs are rewritten consistently in the HTML and its original React root
module: preconnects point to the legacy local origin, and the stylesheet points
to `/__preview__/fonts.css`, a local no-op stylesheet. No fonts are downloaded.
Other external `<link>` tags and `<base>` tags are removed. The guard is prepended
only to `assets/entry.client-COt7fs2Y.js` in memory, leaving the original head
tree and hydration scripts intact. ESM dependencies execute before that prefix;
CSP, server rejection and isolated browser routing therefore enforce the
network/write boundary independently of this convenience guard. Only the exact production API origin
`http://216.106.185.119:8080` is replaced in JS response bytes with the local
legacy origin. Original files are unchanged. `/__preview__/status` exposes only
transform hashes/counts (API URLs, font URLs and entry guard prepend) and preview
status, never source bodies or private paths.

Supported visual entries are `/`, `/dashboard`, and `/dashboard/providers`
(quarantine placeholder by default). The synthetic fixture endpoints are
`GET /portunex/users/me`, `GET /portunex/users/me/stats`,
`GET /portunex/admin/stats`, `GET /portunex/admin/providers`,
`GET /portunex/admin/providers/error-types`, and
`GET /portunex/admin/providers/window-summary`. The Provider response has one
explicitly named synthetic account with no credentials, proxies, or endpoint.
These match reviewed visual consumers, not proven backend behavior. Other API
GETs return explicit 503, all writes 405.
No real login, payment, account action, or upstream execution is available.
Remote fonts are not downloaded; the original CSS uses installed font fallback.

## Synthetic tests

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling:. python3 -m unittest \
  discover -s recovery/tests/frontend -p test_preview.py -v
```

These tests exercise loopback HTTP, pins and transformations using synthetic
assets only. They are not original-page visual or business acceptance tests.
