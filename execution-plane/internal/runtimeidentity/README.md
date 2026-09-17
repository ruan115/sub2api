# Instance-local identity

One small library owns a dedicated 0700 instance directory, a 0600 private
state file, and P-256 CSR generation. No daemon, network, CA key, credential
import, key export or automatic rotation. `cmd/instance-identity` exposes
`init`, `show`, `request`, `install`, with bounded JSON on stdin and public JSON
on stdout; the executable requires Linux UID1000. All errors are fixed strings.

`Binding.AccountHash` uses the existing `provider.RuntimeAccountID` 128-bit
hex representation. The caller supplies authoritative slot/node/epoch/runtime
generation. These labels are not enrollment authentication or an execution
ticket. `pki.Authority.IssueRuntime` can sign the local CSR only after an outer
caller separately authorizes that exact binding. The CSR remains URI-only.
Issuance adds exactly one derived DNS SAN (`Binding.ServerName`) to preserve
normal Go TLS hostname verification, plus the exact URI and serverAuth-only
purpose. The reserved `.invalid` name is not resolved; dialing uses a private IP.
The CA returns a public certificate, never the instance key.

Each newly initialized instance gets independent OS-random key material and a
logical machine ID. The same binding and directory preserve both across tool
or worker process restart. Any binding change fails, never overwrites. All
other directory entries, interrupted init state, malformed/ambiguous state,
unsafe permissions/owner, links and special files fail closed. A file lock
serializes initialization and installation; atomic no-replace initialization
never overwrites. Certificate installation atomically replaces this same state
file, retaining its local key/machine ID; a different installed certificate is
rejected until an explicit rotation lifecycle is implemented.

`InstallCertificate(directory, binding, certificatePEM, trustPEM)` accepts one
leaf, pinned externally supplied CA, exact identity/purpose/time and matching
local public key. It is not an authenticated enrollment endpoint. The CLI form
is `instance-identity install <private-directory> <provisioned-public-CA-file>`;
stdin contains the same five binding fields plus `certificate_pem`. Duplicate
fields, private-key imports, peer-supplied trust and trailing bodies are rejected.
The CA path must be physical, regular, bounded and not shared-writable. Filesystem
root/current UID are trusted; configuring the trust anchor is an operator duty.

`LoadServerTLS` refuses missing certificates. `ServerTLS`/`ClientTLS` require
TLS1.3, a dedicated fixed CA, normal chain/time/EKU/hostname verification and
exact runtime/node SANs. Server client certificates must identify the assigned
node with clientAuth only; runtime certificates cannot impersonate a node.
There is no `InsecureSkipVerify`, system-root fallback, plaintext fallback or
override callback. Session tickets are disabled; live-connection revocation is
not implemented by these constructors. Ticket authorization remains separate.

The existing worker process now requires `EXECUTION_RUNTIME_GENERATION`,
`EXECUTION_IDENTITY_DIRECTORY`, and `EXECUTION_RUNTIME_TRUST_FILE` before it
listens, including fake activation. `Controller.Start` requires node mTLS
materials and completes the actual handshake before returning. Docker currently
uses fixed `/run/execution/identity` and `/run/execution/runtime-ca.pem` paths;
automated CSR/certificate delivery before readiness is not assembled. Do not
run an old plaintext bootstrap or treat TCP health as certificate enrollment.

This assumes a trusted host/kernel and no malicious writer with the same UID
inside that instance. Filesystem permissions are not a boundary against the
host or another process with that UID. The CSR has no private key. Raw private
state must never enter Docker inspect, environment, argv, logs, evidence or Git.
It is not possible to guarantee erasure of all Go heap copies of key material.

Current boundary: the logical machine ID is not `/etc/machine-id` and is not a
claimed reproduction of Claude CLI's unknown internal device-ID behavior.
Tmpfs identity vanishes with the test container. Persistent-volume recovery,
authorized enrollment/delivery, production host-agent assembly, cross-container
mTLS/lease integration, key rotation/revocation and rollback protection need
later slices. S2b proves installed synthetic certificates with the actual Go
worker and Controller on loopback, not the CLI-native bridge. An old home
with an old matching expected binding is not intrinsically proof of freshness.

Reference APIs: [Go rooted filesystem operations](https://pkg.go.dev/os#Root),
[Go CSR creation and verification](https://pkg.go.dev/crypto/x509#CreateCertificateRequest).
