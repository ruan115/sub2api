# Portunex static HTTP wire-evidence status

Status: SSH connectivity restored with explicit `BindInterface=en0`; static asset
collection is still pending. This is an evidence boundary, not an HTTP implementation contract.

Later on 2026-09-13 the user provided a successful SSH command with
`-o BindInterface=en0`. A read-only retry using that option and the same key
authenticated successfully and returned the expected Linux kernel information.
The local default route to the server uses `utun3`; the preceding unbound attempt
was closed before authentication. This points to the local tunnel/egress path,
not a failed SSH key; the precise tunnel policy was not inspected or modified.
Future authorized collection attempts should retain the explicit `en0` option.
This connectivity check did not fetch any of the assets below or change the server.

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

Consequently, a route name in `recovery/contracts/portunex/` is not evidence
for claiming legacy compatibility or exposing a guessed handler under that
legacy path. The separately specified `/__recovery__/v1` synthetic demo is not
an interpretation of these unknown wire fields. A future bounded collection pass needs the
original asset bytes, a hash and secret screen, and a static (non-executing)
review before any wire contract can advance beyond `discovered`.
