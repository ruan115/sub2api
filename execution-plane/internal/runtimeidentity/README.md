# Instance-local identity

One small library owns a dedicated 0700 instance directory, a 0600 private
state file, and P-256 CSR generation. No daemon, network, CA key, credential
import, key export or automatic rotation. `cmd/instance-identity` exposes only
`init`, `show`, `request`, with a bounded binding JSON on stdin and public JSON
on stdout; the executable requires Linux UID1000. All errors are fixed strings.

`Binding.AccountHash` uses the existing `provider.RuntimeAccountID` 128-bit
hex representation. The caller supplies authoritative slot/node/epoch/runtime
generation. These labels are not enrollment authentication or an execution
ticket. `pki.Authority.IssueRuntime` can sign the local CSR only after an outer
caller separately authorizes that exact binding. It returns a URI-only,
serverAuth-only certificate using the existing CA, never the instance key.

Each newly initialized instance gets independent OS-random key material and a
logical machine ID. The same binding and directory preserve both across tool
or worker process restart. Any binding change fails, never overwrites. All
other directory entries, interrupted init state, malformed/ambiguous state,
unsafe permissions/owner, links and special files fail closed. A file lock
serializes initialization; atomic no-replace publication never overwrites.

This assumes a trusted host/kernel and no malicious writer with the same UID
inside that instance. Filesystem permissions are not a boundary against the
host or another process with that UID. The CSR has no private key. Raw private
state must never enter Docker inspect, environment, argv, logs, evidence or Git.
It is not possible to guarantee erasure of all Go heap copies of key material.

Current boundary: the logical machine ID is not `/etc/machine-id` and is not a
claimed reproduction of Claude CLI's unknown internal device-ID behavior.
Tmpfs identity vanishes with the test container. Persistent-volume recovery,
authorized enrollment/atomic certificate installation, worker/host-agent mTLS,
key rotation/revocation and rollback protection need later slices. An old home
with an old matching expected binding is not intrinsically proof of freshness.
Do not enable `InsecureSkipVerify` to use URI-only certificates: future TLS
wiring must preserve chain/time/EKU validation plus exact authorized identity.

Reference APIs: [Go rooted filesystem operations](https://pkg.go.dev/os#Root),
[Go CSR creation and verification](https://pkg.go.dev/crypto/x509#CreateCertificateRequest).
