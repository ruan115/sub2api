# Claude Execution Plane

This module implements the isolated account execution runtime defined by
`docs/prd/claude-execution-plane-v1.md`.

An execution slot is one account's Docker isolation boundary. It is not a cloud
VM. The orchestrator, host-agent and worker are separate processes, and Docker
is hidden behind `internal/provider.ExecutionProvider` so another runtime can be
added without changing placement or account lifecycle semantics.

## Current implementation slice

- slot lifecycle state machine;
- validated pilot timing and capacity defaults;
- Ed25519 execution tickets bound to node/account/slot/epoch;
- worker-side scope and nonce replay guard;
- authenticated worker gRPC Activate/Execute/CountTokens/Health service;
- runnable worker process with explicit fake or default secure activation and drain signal handling;
- provider contract tests and in-memory provider;
- Docker Engine API provider over the host-agent Unix socket, with API negotiation, one internal network per slot, IPAM-gateway host routing and inspect-time sandbox enforcement;
- host-agent runtime controller that obtains signed tickets through a control-plane interface and never owns the signing key;
- `worker_runtime` MySQL 8 migrations plus transactional node/certificate repository contracts;
- one-time digest-only node enrollment, an internal CA, short-lived mTLS identity certificates and rotation;
- fenced node control streams with TLS 1.3, hello/heartbeat timeouts, labels, capacity, bounded command backpressure and session-owned command results;
- reconnecting host-agent NodeControl client with hello/heartbeat reporting, bounded command workers, stable slot-id recovery and fail-closed terminal protocol handling;
- idempotent host-agent slot command execution for create/start/drain/stop/destroy/inspect plus delayed-safe epoch revocation and local slot-capacity enforcement;
- resource-aware stable placement with failure-domain spread and atomic node slot/CPU/memory reservations;
- desired/actual reconciliation with generation-bound idempotent jobs, dispatch retries and atomic command-result closure;
- monotonically increasing execution epochs, Redis-backed short leases, durable MySQL lease ownership and fail-closed protected-egress fencing;
- 15s lease renewal, 45s offline and 90s failover policy with old-epoch connection termination;
- leased at-least-once CCMAX MySQL outbox consumer and generation-fenced projection into slot desired state;
- CCMAX TLS 1.3 mTLS data-plane client contract with epoch/generation-fenced short-TTL route caching and streaming cancellation propagation;
- per-credential-version AES-256-GCM envelope encryption with canonical account/version/auth-type AAD, a fake KMS and a Tencent Cloud KMS API 3.0 adapter restricted to CVM instance-role credentials;
- atomic credential vault version rotation plus digest-only 30-second one-time leases fenced by account/slot/execution epoch/current execution lease, with replay security events;
- host-agent HTTP CONNECT egress with source-IP/slot/epoch binding, exact/wildcard target policy, fixed authenticated HTTP/HTTPS/SOCKS5 upstream conversion and active lease fencing;
- worker onboarding engine for Session Key, OAuth code/PKCE, Setup Token, API Key, Cookie and normalized credential import, with fixed-client upstream verification and secret-redacted results;
- process-local X25519/HKDF/AES-GCM credential transport keys and replay-fenced secure activation that only becomes ready after a credential commit acknowledgement;
- secure oauth_api requests derive Authorization/x-api-key only from the acknowledged in-memory credential;
- authenticated activation packages that bind the orchestrator rotation public key, plus an encrypted worker-return/Vault bridge with lease-and-ciphertext replay fencing;
- control/data-plane/worker protobuf source contracts;
- deterministic fake Claude CLI and fake Anthropic server;
- health endpoints for the three process entry points.
- tagged real-Docker E2E for create/start/activate/messages/count_tokens, direct-egress bypass rejection, drain and destroy.

This is not production-ready yet. The NodeControl client and command executor
are library-complete but not yet wired to role-specific mTLS/Docker process
configuration. The secure worker process is available, while the orchestrator
onboarding command/key exchange, production credential commit endpoint and
CCMAX onboarding entry migration remain gated. CCMAX gateway request dispatch,
host-agent data-plane serving, Claude CLI runtime image and deployment
automation are also incomplete.

## Fixed proxy egress boundary

Workers receive only an internal, credential-free HTTP proxy URL such as
`http://host-agent.execution.internal:18080`. The host-agent maps the worker's
private source IP to one slot, execution epoch and proxy lease, then validates
the current execution lease before dialing the allowed upstream target. Remote
proxy usernames and passwords remain in the host-agent and all string/JSON
representations redact them.

The gateway converts CONNECT through authenticated HTTP, TLS-verified HTTPS or
SOCKS5 proxies. Same-epoch proxy replacement is rejected, HTTPS verification
cannot be disabled, response headers are bounded, and periodic lease
revalidation closes existing tunnels when the epoch is revoked or the lease
backend becomes unavailable. The tagged Docker E2E additionally runs a literal
TCP probe inside the worker: the host-agent proxy path succeeds while a direct
connection to a listener outside the slot bridge is unreachable.

## Worker onboarding boundary

The worker onboarding engine accepts six explicit source types: Session Key,
OAuth code plus PKCE verifier, Setup Token, API Key, Cookie containing a
`sessionKey`, and an existing normalized credential import. Session Key and
Cookie inputs perform organization lookup, authorization and AT/RT exchange;
OAuth code performs token exchange; Setup Token/API Key/import paths are
verified upstream before activation. Public non-TLS endpoints, redirects,
oversized material, malformed responses and authentication failures fail
closed. Cloudflare challenge retries are bounded and context-aware.

The host-agent may start a worker without activating it, request that process's
X25519 public key with a one-time execution ticket, and relay a bundle sealed by
the orchestrator. Its AEAD context binds the runtime account hash, slot, epoch,
credential lease, proxy lease and purpose. The bundle also authenticates the
orchestrator's rotation recipient key, so the host-agent cannot substitute a
key it controls. The normalized credential is sealed back to that recipient;
the orchestrator checks the current slot/epoch/execution/proxy lease and a
credential-lease+ciphertext replay fence before rotating the KMS-backed vault.
Secure activation switches the in-memory credential only after the commit
acknowledgement. A bidirectional worker RPC carries this handshake and the
host-agent validates every binding before forwarding ciphertext. Lost
acknowledgements reuse the pending normalized result without repeating the
upstream login; the orchestrator fences the canonical material digest rather
than randomized transport ciphertext. The host-agent now maintains the
NodeControl stream and executes fenced Docker slot lifecycle commands, and the
worker binary can run the secure activator. The orchestrator-to-host-agent
onboarding command/key exchange and CCMAX handler migration remain part of task
5.5; these components alone do not enable the new onboarding path.

## Local checks

```sh
make test
make test-race
make vet
make docker-e2e
```

The gated repository integrations use disposable databases supplied by the
caller:

```sh
EXECUTION_MYSQL_TEST_DSN='...' go test ./internal/runtime/store -run TestMySQLRepositoryIntegration
EXECUTION_REDIS_TEST_URL='redis://127.0.0.1:6379/0' go test ./internal/lease -run TestRedisBackendIntegration
```

## Credential encryption boundary

`internal/credential.Service` requests a new 256-bit data key for every
credential version, encrypts locally with AES-256-GCM and binds both the local
ciphertext and wrapped data key to canonical JSON containing account ID,
credential version and auth type. Only ciphertext, the opaque encrypted DEK, a
12-byte nonce, AAD JSON and KMS key metadata map to `credential_versions`.

The production Tencent Cloud constructor uses KMS API `2019-01-18` and only
`CvmRoleProvider`; it does not use the SDK default environment/profile fallback
chain. `TencentKMSConfig.KeyVersion` is an operator-controlled key-version label
persisted with each envelope, while Tencent's opaque ciphertext remains the
authoritative material-version reference. The fake KMS is for local tests only
and is not wired into production configuration. Worker processes do not import
or construct either KMS implementation.

`credential.Vault.Rotate` serializes same-process account rotations and the
MySQL repository adds a monotonic version fence for multiple orchestrator
replicas. New ciphertext insertion, active-version switch and revocation of old
unconsumed leases commit in one transaction. Lease tokens contain 256 random
bits; only their SHA-256 digest is persisted. Issue and redeem both revalidate
the current slot assignment, node, execution epoch and unexpired execution
lease. Redeem marks the token consumed before KMS decryption, so a KMS failure
cannot turn one token into a retryable credential oracle.

MySQL normalizes native JSON column formatting. Stored AAD is therefore parsed
with unknown fields rejected, compared field-for-field with the authoritative
account/version/auth type, and rebuilt into the sole canonical byte sequence
before AES-GCM/KMS verification. Semantic changes remain fail-closed. Lease
grants and leased material redact secrets in string and JSON serialization, and
callers must invoke `LeasedCredential.Destroy` immediately after copying the
credential into worker memory/tmpfs.

## Slot route handoff

The orchestrator publishes a ready slot as a Redis hash at
`execution:route:v1:<slot_id>` with a mandatory Redis TTL. The hash fields are
`slot_id`, `node_id`, `endpoint`, `execution_epoch` and `route_generation`.
`endpoint` must be a private WireGuard/loopback IP and port. CCMAX additionally
caps its process-local copy at 10 seconds and sends slot/epoch/generation on
every RPC, so an expired or replaced assignment cannot be silently reused.

`make docker-e2e` uses the Docker Engine API for runtime operations. On Colima,
the test binary runs inside the Linux VM so it exercises the same per-slot
bridge/gateway topology as a Linux host; Docker CLI is used only to build the
local scratch test image.

Buf is intentionally not taken from an implicit host installation.
`make proto-lint` and `make proto-generate` use the exact version declared in
the Makefile and the remote plugin versions pinned in `buf.gen.yaml`.

No command in this module deploys to `43.172.83.39`; remote mutation requires a
separate explicit approval and the deployment gate in `verification.md`.
