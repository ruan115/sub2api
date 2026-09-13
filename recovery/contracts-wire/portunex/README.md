# Portunex static HTTP wire-evidence status

Status: the first static collection and source-anchored observations are complete
(2026-09-13). This is an evidence boundary, not a verified HTTP implementation contract.

Later on 2026-09-13 the user provided a successful SSH command with
`-o BindInterface=en0`. A read-only retry using that option and the same key
authenticated successfully and returned the expected Linux kernel information.
The local default route to the server uses `utun3`; the preceding unbound attempt
was closed before authentication. This points to the local tunnel/egress path,
not a failed SSH key; the precise tunnel policy was not inspected or modified.
Future authorized collection attempts should retain the explicit `en0` option.
That earlier connectivity-only check fetched no assets. The later collection below
read static files and catalog metadata without changing production services or data.

## Completed first collection

Twelve explicitly named assets were downloaded into a new repository-external
private directory. Every local size/SHA-256 matched its read-only remote preflight.
Eleven files (163,689 bytes) passed the full-file recognisable-secret screen and
were preserved with the existing evidence tool into a second new private directory.
The preservation was independently verified; original scripts were never executed.

`_dashboard.providers-ABk0JmwU.js` (96,456 bytes, SHA-256
`72377588f9e0e4e15d97c1f9df038fa495052ab0e10051df62d1d72daedbf413`)
hit two credential-shaped URL patterns. It remains excluded pending separate
review; no matching values or original bundle contents are committed. The
Provider list helper is a different file and passed screening; its page filter
and pagination semantics remain unknown. No safety rule was disabled.

The allowlist is [static-wire-artifacts.json](../../baselines/portunex/static-wire-artifacts.json).
Original bytes and the excluded file remain outside Git. The downloaded subset
is not a backup of all 150 assets or the whole system.

| Module | Observations | Anchors | API records | Main client-side finding |
| --- | --- | --- | --- | --- |
| [identity](identity/observations.json) | 9 | 21 | 2 | login/logout use POST; login consumes token/user; localStorage and Bearer authentication |
| [users](users/observations.json) | 7 | 15 | 2 | users/me GET returns the directly consumed user; admin list consumes users/total |
| [apikeys](apikeys/observations.json) | 7 | 17 | 2 | separate user/admin GET collections; offset/limit and api_keys/total |
| [providers](providers/observations.json) | 4 | 7 | 1 | administrative GET helper; providers array inferred only from optimistic list-cache update |

All four documents pass `wire verify`: **27 observations, 60 anchors, 7 API records**,
always `source_anchored` and `business_verification=false`. Existing discovery
catalog entries and unknowns were not promoted or cleared. Review corrected one
wording error: the users page trims id/email but sends role unchanged when not `all`.

The client stores `portunex_token` and sends `Authorization: Bearer ...`; the
synthetic cookie-session demo is not an interpretation of this old contract.
Cookie/XSRF behavior, full response schemas, permissions and token lifecycle still
require separate evidence. A failed logout may also clear the token in the shared
401 interceptor; the lower helper's success-only clear is not the entire failure path.

## Reproduce the source checks

Use the explicitly retained source directory, or the `files/` directory in the
verified preservation. Replace the placeholder; this does not contact production.

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit wire verify \
  --observations recovery/contracts-wire/portunex/identity/observations.json \
  --manifest recovery/baselines/portunex/static-wire-artifacts.json \
  --source-root /absolute/private/verified-evidence/files \
  --catalog-root recovery/contracts
```

Repeat for `users`, `apikeys`, `providers`. Full-source verification intentionally
requires private original bytes; CI must not substitute synthetic bytes and claim
these production observations passed. Existing synthetic tests check validator safety.

The narrow database catalog result is recorded separately in
[postgres/README.md](../../baselines/portunex/postgres/README.md); the next compatible
authentication plan is [documented here](../../docs/portunex-identity-next-slice.md).

## Earlier failed collection attempt

On 2026-09-13, the recovery workflow attempted to retrieve only these explicitly
scoped production static assets:

- `/opt/gateway/public/assets/auth-store-C1tFGCSg.js`
- `/opt/gateway/public/assets/admin-qirDwatZ.js`
- `/opt/gateway/public/assets/use-api-keys-_Qqurcxx.js`
- `/opt/gateway/public/assets/use-providers-HlnM_V-l.js`
- `/opt/gateway/public/assets/manifest-c2858866.js`

The dedicated SSH attempt was closed during key exchange, before authentication
or any file read.  The narrowly scoped public HTTP and HTTPS manifest checks
also returned no asset bytes.  No source bundle, hash, secret-screen result, or
external private copy was created.  The original assets must not be added to
Git if later retrieved.

The first-phase catalog records route *discovery* from earlier static analysis.
It does **not** establish the following wire details for login, session, user,
API-key, or provider endpoints:

- HTTP method and per-route body shape;
- cookie / credential inclusion and request headers;
- response envelope, error representation, and pagination fields;
- client-consumed response fields and mutation semantics.

Consequently, a route name in `recovery/contracts/portunex/` alone is not evidence
for claiming legacy compatibility or exposing a guessed handler under that
legacy path. The separately specified `/__recovery__/v1` synthetic demo is not
an interpretation of unknown wire fields. The completed collection above adds
static source anchors, not server-side compatibility acceptance.
