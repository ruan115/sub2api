# Verification Plan

## Required Gates

| Gate | Evidence |
|---|---|
| Existing CCMAX behavior | `cd ccmax-manager && go test ./...` |
| Execution plane correctness | `cd execution-plane && go test ./...` |
| Race safety | `cd execution-plane && go test -race ./...` |
| Static checks | `cd execution-plane && go vet ./...` |
| UI | frozen install + typecheck + Vitest |
| Docker sandbox | provider inspect assertions + attempted bypass tests |
| Fixed proxy | HTTP/HTTPS/SOCKS5 fake proxy captures, no direct upstream connection |
| No double-active | node partition/expiry/rejoin test with epoch evidence |
| Credential secrecy | log/DB/Redis/Docker inspect/filesystem scan |
| Protocol | Messages stream/nonstream, tools, thinking, count_tokens, models, Chat Completions |
| Queue | 1000 outstanding, per-key fairness, timeout/reject/cancel |
| Lifecycle | provision/drain/archive/delete/restore/purge/update |
| Rollout | canary failure pauses, old digest preserved, rollback succeeds |
| DR | MySQL point-in-time restore and runtime reconciliation |

## Performance Targets

- oauth_api gateway overhead p95 < 100 ms with fake upstream.
- CLI startup overhead p95 < 1.5 s on approved production-class node.
- Node failover < 120 s.
- 1000 long-lived requests without OOM, deadlock or unbounded waiter growth.
- Client cancellation releases queue/gRPC/worker/CLI resources.

## Security Invariants

- Plain credentials never appear in CCMAX DB after account migration.
- Plain credentials never appear in logs, audit JSON, Redis, Docker env/inspect or COS.
- Worker cannot access Docker Socket, host paths, sibling slot network or public Internet directly.
- Only current epoch can open/maintain protected egress.
- KMS permissions exist only on orchestrator control plane.
- Body viewing requires explicit permission and always creates an audit record.

## Remote Deployment Gate

Local code completion does not authorize server changes. Before any non-check-mode Ansible run against `43.172.83.39`, record:

1. explicit deployment approval;
2. playbook `--check --diff` output;
3. existing 80/443/UFW preservation plan;
4. rollback commands;
5. backup/restore evidence.

## Implementation Evidence

### Slice 1 — skeleton, contracts and local Docker provider (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Execution-plane unit tests | PASS | `cd execution-plane && go test ./...` |
| Race detector | PASS | `cd execution-plane && go test -race ./...` |
| Static analysis | PASS | `cd execution-plane && go vet ./...` |
| Binary build | PASS | orchestrator, host-agent, worker, fakeclaude and fakeanthropic packages |
| Docker sandbox contract | PASS (Engine API payload-level) | per-slot internal network; API version negotiation; non-root/read-only/no-new-privileges/cap-drop/PID/CPU/memory/tmpfs; allowlisted `seccomp=builtin` and `apparmor=docker-default`; account plaintext, bind mounts and worker Docker socket absent |
| Ticket isolation | PASS | Ed25519 verification; node/account/slot/epoch/scope binding; concurrent nonce replay permits exactly one use |
| Worker local gRPC | PASS | bufconn E2E covers activation, execute begin, count_tokens, mode health, replay and wrong-epoch rejection |
| Frontend typecheck baseline | PASS | pnpm `10.34.5`, frozen lockfile |
| Frontend Vitest baseline | KNOWN FAIL | 1629/1630 tests pass; unrelated pre-existing Grok placeholder source assertion fails |
| Proto lint/generation | PASS | pinned Buf `v1.72.0`; `make proto-lint`, remote codegen and `make proto-check` drift test pass |

No Docker daemon, real upstream, real credential, KMS, Redis, MySQL, COS or remote server was touched by this slice.

### Slice 2 — real local Docker worker loop (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Review regressions | PASS | concurrent Docker create conflicts reconcile safely; empty IDs are re-inspected/cleaned; unclassified executor errors are redacted |
| Inspect-time sandbox | PASS | rejects root/writable rootfs/missing cap-drop/unapproved seccomp or AppArmor/resource gaps/tmpfs gaps/init gaps/binds, mounts, published ports and secret-named env fields |
| Worker process | PASS | real gRPC process loads opaque account hash, public verification key, slot/node/epoch; production activation fails closed; `SIGUSR1` enters drain |
| Control-plane key boundary | PASS | host-agent consumes a `TicketSource`; signing private key exists only in the local E2E test implementation |
| Docker network topology | PASS | `Internal=true` per-slot bridge; host-agent name resolves to that network's private IPAM gateway, not Docker's unrelated default `host-gateway` |
| Docker E2E | PASS | `make docker-e2e`: create → start → health → signed activate → Messages → count_tokens → drain → destroy against fake proxy/upstream |
| Docker inspect secrecy | PASS | raw account ID and encrypted activation bundle absent from container env/labels; no credential/token env names |
| Execution-plane gates | PASS | `make check` (unit, race, vet, Buf lint/generated drift) |

Local Colima was started for this verification. The test used no real account,
credential or upstream. No remote server or deployment state was changed.

### Slice 3 — node identity and control stream (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| `worker_runtime` schema | PASS | MySQL `8.4` disposable local container accepted `up → down → up`; 15 tables and 13 same-database foreign keys; active slot assignment uniqueness rejected a duplicate |
| Repository transactions | PASS | one-time enrollment digest consumption, node/certificate atomic commit, certificate replacement rollback boundary and session-fenced disconnect tests |
| Enrollment secrecy | PASS | 256-bit token returned once; only SHA-256 digest reaches repository/schema; reused or expired/wrong-node enrollment has one generic rejection |
| Internal PKI | PASS | P-256 CA loading/key match, client/server EKU verification, SPIFFE-style node identity and canonical serial; TLS 1.3 server verifies optional client cert only so enrollment remains possible while protected RPCs enforce identity |
| Certificate rotation | PASS | renewal requires an active matching mTLS identity and rotation window; previous serial becomes inactive; active streams revalidate the certificate on every event |
| Node control stream | PASS | real bufconn gRPC/TLS enrollment → Hello → heartbeat → command → result → disconnect → rotated-cert reconnect integration test |
| Stream safety | PASS | first-Hello and heartbeat timeouts, server-received liveness time, declared-capacity enforcement, duplicate stream rejection, DB `control_session_id` fencing, bounded asynchronous send backpressure, pending-command ownership and error redaction |
| Execution-plane gates | PASS | `make check` (unit, race, vet, Buf lint/generated drift) |

The MySQL container was removed and local Colima was returned to its original
stopped state. No real credential, upstream, project database or remote server
was touched.

### Slice 4 — placement, reconciliation and execution fencing (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Resource-aware placement | PASS | connected/session-fresh/capability/image/label/slot/CLI/API/total/CPU/memory filters; stable current placement; failure-domain spread; projected least-load and deterministic node-ID tie break |
| Atomic slot reservation | PASS | MySQL transaction locks desired slot and reserves slot/CPU/memory against the current node session; 12 concurrent in-memory contenders cannot exceed six available reservations; active assignment and monotonically increasing epoch are unique |
| Desired/actual reconciliation | PASS | state planner covers ready/drained/absent and image replacement; 32 concurrent reconciles claim one stable job/command; failed dispatch retries the same command ID after backoff |
| Command result closure | PASS | issued-command ownership, node/slot/epoch fencing, assignment observation and job completion/failure commit atomically; mismatched and stale results are rejected |
| Execution leases | PASS | Redis Lua acquire/renew/validate/revoke; durable MySQL lease binds current assignment node/epoch/owner; default 15s renew, 45s offline and 90s failover boundaries |
| No double-active | PASS | 89s failover rejected; at 90s old assignment is force-fenced/released, old protected connection closes, node B receives epoch 2, and epoch 1 remains invalid |
| Redis fail-closed | PASS | disposable Redis `7.4` integration covers conflict, TTL expiry, epoch replacement and revoke; a closed Redis client rejects new validation and closes existing protected egress |
| MySQL repository integration | PASS | disposable MySQL `8.4` accepted `up → down → up`; enrollment → heartbeat → reservation → job/result → lease → release → epoch 2; database rejects capacity overflow |
| Execution-plane gates | PASS | `make check` (unit, race, vet, Buf lint/generated drift) |

The MySQL and Redis containers were disposable and contained only generated test
identifiers. No real account, credential, upstream, project database or remote
server was touched. Local Colima was stopped after verification.

### Slice 5 — CCMAX transactional outbox foundation (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| CCMAX execution schema | PASS | existing accounts default to `legacy`; account allowed/preferred modes, migration/runtime status, stable slot/provider/epoch, 1/3/3 limits; group policy/queue/image channel; isolated mode health; runtime audit |
| Transactional outbox | PASS | runtime generation/status/slot update, secret-free event insert and operation audit commit in one serializable transaction; rejected payload or migrated-with-plaintext condition rolls the entire transaction back |
| At-least-once checkpoint | PASS | ordered sequence, owner + lease fencing, nack replay, expired-owner takeover, stale-owner rejection and idempotent ack; 32 concurrent memory contenders produce one checkpoint owner |
| Orchestrator projection | PASS | execution-plane consumer validates secret-free events and projects account/generation into idempotent slot desired state; stale generations are rejected |
| No plaintext legacy fallback | PASS (routing foundation) | migrated/migrating accounts are excluded from legacy gateway selection, sticky recovery, reserve activation/capacity, pool resolve, token refresh, account health and direct quota refresh |
| SQLite compatibility | PASS | fresh schema and incremental execution-feature migration tests; SQLite→MySQL migration list includes mode health/outbox/audit without applying the incompatible `id` append watermark to `sequence` |
| Real MySQL cross-module integration | PASS | disposable MySQL `8.4`: CCMAX migration is idempotent, writes event/checkpoint tables, then execution-plane claims → nacks → replays → acks from the same database |
| CCMAX gates | PASS | `go test ./...` (52.6s), `go test -race ./...` (325.4s), `go vet ./...`, tagged SQLite migration tests |
| Execution-plane gates | PASS | `make check` (unit, race, vet, Buf lint/generated drift) |

The MySQL container was removed and local Colima was stopped. Events contain
generated identifiers and policy metadata only; payload validation rejects
credential/token/cookie/password/proxy URL fields and token-shaped strings.
No real account, credential, upstream, project database or remote server was
touched.

### Slice 6 — CCMAX mTLS data-plane client (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Shared wire contract | PASS | `BeginExecution` and `CountTokensRequest` carry stable slot ID, execution epoch and route generation; CCMAX stubs are mechanically synchronized from the canonical execution-plane generated files and covered by Buf drift checks |
| TLS 1.3 mutual authentication | PASS | real loopback TCP gRPC server requires and verifies a client certificate; CCMAX verifies the internal CA/server name, presents its client certificate and supports certificate-file rotation on new handshakes |
| Route cache and fencing | PASS | Redis route resolver requires a live TTL; local cache TTL is capped at 10s, bounded to 2048 entries, accepts private IP endpoints only and rejects old epoch/generation or same-epoch node/endpoint conflicts |
| Streaming and cancellation | PASS | CCMAX returns the gRPC stream without response buffering; downstream context cancellation reaches the server as `context.Canceled` and no route invalidation is triggered by a normal client cancel |
| Secret boundary | PASS | downstream authorization/API-key headers cannot enter the data-plane request; only the explicit safe header allowlist is accepted |
| CCMAX gates | PASS | `go test ./...`, targeted execution-client race tests and `go vet ./...` |
| Execution-plane gates | PASS | Buf lint/generated drift, `go test ./...` and `go vet ./...` |

This slice is guarded by `CCMAX_EXECUTION_DATAPLANE_ENABLED` and does not route
legacy accounts through the new data plane. The test used generated certificates
and request bodies only; no real account, credential, upstream, project database
or remote server was touched.

### Slice 7 — execution feature flags and one-way ownership (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Group settings API | PASS | admin `GET/PUT /api/groups/{id}/execution` validates `auto/cli_only/api_only`, `queue/reject` and `stable/canary`; existing groups retain `auto + queue + stable` defaults |
| Account settings API | PASS | admin `GET/PUT /api/accounts/{id}/execution` validates non-empty allowed modes, preferred-mode membership and 1..1000 per-mode/total limits; migration/runtime/slot/epoch and isolated mode health are read-only |
| Legacy default | PASS | existing accounts remain `execution_migration_status=legacy`; the dispatch decision returns legacy regardless of configured preferred modes until a one-way migration occurs |
| New-plane gating | PASS | migrated accounts require the global mTLS client, `runtime=ready`, non-zero generation/epoch/slot and a healthy mode allowed by both account and group policy |
| No silent fallback | PASS | migrated/migrating accounts return an explicit unavailable decision when data-plane/runtime/mode health is unavailable; they are never returned to the legacy route |
| Credential ownership | PASS | account edit, OAuth URL/exchange, Session Key auth, direct save, batch same-email reauthorization, background refresh/reauth and subscription backfill all require `legacy`; final credential writes repeat the migration predicate to close read/write races |
| Regression gates | PASS | CCMAX full tests, targeted execution/migration race tests, vet and tagged SQLite migration tests; execution-plane full unit/race/vet/Buf gate |

These settings do not yet send production gateway traffic through a host-agent;
that remains in the oauth_api data-plane tasks. No real account, credential,
upstream, project database or remote server was touched.

### Slice 8 — credential envelope encryption (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Independent envelope keys | PASS | every seal obtains a new 32-byte DEK and 12-byte nonce; repeated plaintext produces different ciphertext and encrypted DEK |
| Authenticated metadata | PASS | canonical JSON binds schema, account ID, credential version and auth type to both AES-GCM and KMS encryption context; account/version/type/AAD tampering is rejected |
| AES-256-GCM integrity | PASS | ciphertext and nonce tampering fails closed; plaintext size is bounded and never copied into the envelope fields |
| DEK memory lifetime | PASS | generated and decrypted plaintext DEK slices are erased on success, validation failure and provider-error paths |
| Tencent Cloud KMS | PASS (adapter-level) | official API 3.0 SDK requests `GenerateDataKey` with `AES_256`, 32 bytes, non-hosted data key and encryption context; `Decrypt` repeats the context and validates CMK identity |
| Instance-role boundary | PASS | production constructor uses only `CvmRoleProvider`; it does not enable the SDK environment/profile/default provider chain or accept long-lived secrets in config |
| Fake KMS and fault injection | PASS | local AES-GCM wrapping mirrors key/AAD binding; generate/decrypt failure, wrong key version, wrong AAD and corrupted wrapped key all return sanitized fail-closed errors |
| Execution-plane gates | PASS | `make check` (full unit, race, vet, Buf lint/generated drift) |

No real KMS endpoint, CAM role, account credential, upstream, project database,
container or remote server was accessed. Tencent behavior was validated through
the official SDK request/response types with an in-process fake API client.

### Slice 9 — credential vault, rotation and one-time leases (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Atomic version rotation | PASS | account vault lock + monotonic version fence; ciphertext insert, active pointer/auth-type switch and old outstanding-lease revoke share one MySQL transaction; insert failure rolls back before active switch |
| Multi-replica lock order | PASS | issue/consume/rotation use the fixed order `vault → credential lease → assignment/execution lease`, avoiding the prior lease/vault inversion |
| Lease secrecy | PASS | 256-bit random `clt_` token is returned once; only SHA-256 reaches `credential_leases`; string/JSON serialization redacts lease token and plaintext material |
| Runtime fencing | PASS | issue and consume require matching account/slot/current assignment epoch, matching assignment/execution-lease node, unrevoked execution lease and active credential version |
| Exactly-once redemption | PASS | 32 concurrent callers yield exactly one plaintext result and 31 generic rejections; 20 repeated race runs pass |
| Replay/epoch audit | PASS | replay, expired/revoked/binding/clock and inactive epoch/version paths fail closed and commit credential-free `credential_security_events` records |
| KMS failure semantics | PASS | token is atomically consumed before decrypt; a KMS failure returns no material and the same token cannot be retried |
| MySQL JSON AAD | PASS | native JSON key/whitespace normalization is accepted only after strict field validation and canonical reconstruction; unknown/trailing/changed fields are rejected |
| Real MySQL integration | PASS | disposable MySQL `8.4` accepted `up → down → up`, then version rotate → lease issue → redeem/decrypt → replay/audit with real JSON/BLOB scans |
| Execution-plane gates | PASS | repeated credential race tests plus `make check` (full unit, race, vet, Buf lint/generated drift) |

The disposable MySQL container was removed, Colima was returned to its original
stopped state and its original 100 GiB disk configuration was restored. Tests
used generated fake credential strings and fake KMS material only; no real KMS,
account, upstream, project database or remote server was touched.

### Slice 10 — fixed proxy CONNECT egress (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Worker-facing boundary | PASS | source private IP resolves to one slot/current epoch; workers receive only the credential-free host-agent endpoint; proxy string/JSON forms redact username and password |
| HTTP proxy conversion | PASS | real TCP CONNECT tunnel through an authenticated in-process HTTP proxy preserves bidirectional payloads and captures the expected target/auth header |
| HTTPS proxy conversion | PASS | the same tunnel passes through a TLS proxy with certificate verification; insecure verification is rejected and ALPN is fixed to HTTP/1.1 |
| SOCKS5 conversion | PASS | authenticated SOCKS5 negotiation, target conversion and bidirectional echo complete through a real TCP listener |
| Target policy | PASS | canonical host/port matching supports exact and subdomain wildcard entries; malformed, wrong-port and non-allowlisted targets fail before proxy dialing |
| Epoch/proxy fencing | PASS | same-epoch fixed-proxy replacement is rejected; a revoked execution lease closes an active tunnel; unknown sources and unavailable lease backends fail closed |
| Defensive limits | PASS | CONNECT request/response headers, proxy URL/credentials, target count and tunnel duration are bounded; remote errors are sanitized |
| Race and full gates | PASS | targeted proxy tests passed under `-race -count=20`; `make check` passed full unit, race, vet, Buf lint and generated-code drift gates |

All upstreams were disposable loopback echo/proxy servers with generated test
credentials. No Docker daemon, public upstream, real proxy, account credential,
project database or remote server was touched by this slice.

### Slice 11 — worker network bypass rejection (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Internal slot network | PASS | provider creates one non-attachable Docker bridge per slot with `Internal=true`; inspect-time validation rejects replacement with a non-internal or foreign-owned network |
| Authorized proxy path | PASS | real worker container completed Messages and count_tokens through `host-agent.execution.internal`; fake host proxy observed exactly two requests |
| Direct bypass probe | PASS | a literal-IP TCP probe executed inside the same worker container, without proxy environment use, could not reach a listening address outside the slot bridge; the target accepted no connection |
| Test integrity | PASS | probe runs non-root from a helper present only in the local E2E image; result/exit status are collected through attached Docker exec with bounded multiplexed output |
| Sandbox continuity | PASS | worker remains non-root, read-only, cap-drop ALL, no-new-privileges, resource-limited, volume/socket-free and has no published ports or proxy credentials |
| Cleanup | PASS | slot container/network and generated E2E image were removed; Colima was stopped and retained its original 100 GiB disk configuration |

`make docker-e2e` passed against the local Colima Docker Engine. The test used a
fake worker activation bundle, a local fake proxy/upstream and a local TCP
listener only. No public endpoint, real credential, project database or remote
server was accessed.

### Slice 12 — secure worker onboarding foundation (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Onboarding source coverage | PASS (worker library) | Session Key, Cookie, OAuth code/PKCE, Setup Token, API Key and strict normalized credential import each complete against a fake upstream through an explicit HTTP proxy |
| Fixed-exit behavior | PASS | tests use unreachable literal target URLs and only succeed because every organization/authorize/token/profile/models request traverses the configured proxy transport |
| OAuth safety | PASS | authorization redirects are disabled; programmatic Session Key flow validates callback origin/path and state; Cloudflare challenge retry is bounded and context-cancellable |
| Secret handling | PASS | typed input/result/active/commit structures redact string and JSON forms; caller-owned source, PKCE, normalized credential and decrypted transport buffers have explicit zeroing paths |
| Worker-only transport | PASS | process-local X25519 recipient key; HKDF-SHA256 + AES-256-GCM envelope binds runtime account hash, slot, epoch, credential lease, proxy lease and purpose; tamper/wrong-key/wrong-context fail closed |
| Replay and commit ordering | PASS | 32 concurrent activations of one sealed lease yield one onboarding/commit; active credentials switch only after the commit interface acknowledges a version ID; commit errors remain secret-free and leave no active credential |
| Two-stage host flow | PASS (protocol/library) | ticket-protected worker RPC returns the process public key; host-agent `Start` supports unactivated connection before sealed `Activate`; protobuf generation/drift checks pass |
| Race and full gates | PASS | onboarding and transport suites passed repeated `-race -count=20`; `make check` passed full unit, race, vet, Buf lint and generated-code drift gates |

This slice is foundation, not a completed production onboarding path. OpenSpec
task 5.5 remains unchecked until the encrypted vault commit bridge and CCMAX
single/batch handlers use the two-stage worker flow. Tests used generated fake
secrets and loopback servers only; no real Anthropic, account, proxy, KMS,
project database, Docker daemon or remote server was accessed.

### Slice 13 — encrypted onboarding Vault return bridge (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Recipient substitution defense | PASS | the orchestrator rotation key id/public key and onboarding material share one worker-sealed activation package; mismatched key bytes and package tampering fail closed |
| Worker return secrecy | PASS (library) | normalized OAuth/Setup/API material is binary-framed then sealed with X25519/HKDF-SHA256/AES-256-GCM; host-agent-facing string/JSON forms omit ciphertext and plaintext |
| Orchestrator authorization boundary | PASS (interface + sink) | the commit transaction contract requires current account/slot/epoch, execution lease, proxy lease and one-time credential lease validation before KMS/Vault rotation |
| Replay fencing | PASS | credential lease plus SHA-256 canonical material identity is idempotent across fresh transport encryption and rejects different credential material under the same lease without a second rotation |
| Account identity binding | PASS | only the authorizer resolves the source account id; its 128-bit runtime digest must match the AEAD account binding before decrypt/rotate |
| Maximum credential boundary | PASS | a full 1 MiB normalized credential plus binary framing survives seal/open; transport/RPC limits reserve bounded framing/base64 overhead |
| Failure secrecy | PASS | recipient, transport, authorizer and Vault failures collapse to stable credential-free errors; decrypted/frame buffers have explicit erase paths |
| Race and full gates | PASS | targeted credential/worker/service suites passed `-race -count=20`; full `make check` passed unit, race, vet, Buf lint and generated-code drift |

Task 5.5 remains unchecked: host-agent control-command/process wiring and CCMAX
single/batch onboarding handlers do not yet use the two-stage flow. All tests
used generated fake secrets and local in-process components; no public
upstream, real credential, project database, Docker daemon or remote server was
accessed.

### Slice 14 — bidirectional secure activation acknowledgement (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Additive worker protocol | PASS | new `SecureActivate` bidi stream preserves the existing unary fake/local RPC; generated Go and CCMAX stubs are synchronized and Buf drift/lint gates pass |
| Stream ordering | PASS | ticket-protected begin → one sealed credential commit → Vault version acknowledgement → completed; missing/wrong events and invalid version ids fail closed |
| Host-agent opacity | PASS | host-agent validates account hash, slot, epoch, credential lease and proxy lease, then forwards only the sealed bundle to the orchestrator sink |
| Commit-before-ready | PASS | worker active credential and healthy mode remain unset until the orchestrator sink returns a version and the host-agent sends its acknowledgement |
| Vault failure | PASS | failed authorization/KMS/Vault commit produces no acknowledgement, returns a credential-free error and leaves the worker inactive |
| Lost acknowledgement recovery | PASS | worker retains one bounded pending normalized result in memory, retries the identical activation without a second upstream onboarding call, and erases it on success/drain/superseding lease |
| Randomized retry idempotency | PASS | a freshly X25519/AES-GCM-enveloped copy of identical canonical material returns the recorded version without a second Vault rotation; different material under the lease is rejected |
| Integrated local bridge | PASS | bufconn E2E executes worker secure onboarding → encrypted stream → host-agent → orchestrator rotation sink → version ACK → worker active credential |
| Regression gates | PASS | secure worker/service/host-agent tests passed `-race -count=20`; execution-plane `make check` and CCMAX `go test ./...` passed |

This is still not a deployable onboarding path: the long-lived node control
client/command runner does not yet invoke the secure activation stream, and
CCMAX does not yet submit new execution-plane onboarding operations. No real
upstream, credential, proxy, KMS, database, Docker daemon or remote host was
used.

### Slice 15 — review hardening, NodeControl client and secure worker process (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Review findings | PASS | secure activation treats `completed` as the terminal event instead of waiting indefinitely for EOF; client/server cancellation preserves canceled/deadline semantics; worker/host/orchestrator share canonical version-id validation |
| Vault callback fencing | PASS | one authorization transaction can invoke its Vault rotation callback at most once; a repeated callback is rejected before a second credential version is created |
| Stable slot recovery | PASS | additive provider `InspectSlot` resolves a provider runtime from stable `slot_id`; a fresh host-agent executor recovers and controls a pre-existing slot without a persisted container id |
| Node command execution | PASS | create/start/drain/stop/destroy/inspect are deadline-bound, epoch-exact and idempotent; local slot capacity fails closed and delayed old-epoch revocation cannot stop a newer generation |
| Long-lived control stream | PASS (library) | sorted hello capabilities, immediate/periodic heartbeat, bounded command workers/results, cancellation and bounded reconnect backoff are covered over a real bufconn bidi gRPC stream |
| Secure worker process | PASS | `fake=true` retains the tagged local E2E path; `fake=false` constructs a process-local recipient, real onboarding engine and secure activator; oauth/setup/API-key headers are derived only from the acknowledged in-memory credential with strict normalized JSON validation |
| Repeated race tests | PASS | host-agent and worker suites passed `-race -count=10`; the earlier credential/service/provider hardening suites also passed repeated race runs |
| Full regression gates | PASS | execution-plane `make check` passed unit, full race, vet, Buf lint and generated drift; CCMAX `go test ./...` and repository diff checks passed |

Task 5.5 remains unchecked. The NodeControl protocol still needs an additive
orchestrator/host-agent key-discovery and encrypted activation command, and the
CCMAX single/batch handlers have not migrated. Tests used fake providers,
generated keys/secrets and in-process gRPC only; no Docker daemon, public
upstream, project database or remote server was accessed.

### Slice 16 — NodeControl secure onboarding bridge (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Additive control protocol | PASS | protocol minor 1 adds dedicated credential-key, secure-activation, encrypted commit and version-ACK events without reusing existing fields; minor 0 lifecycle nodes remain accepted but cannot receive the new commands |
| Capability/dependency gating | PASS | dispatch requires the node-advertised `secure_activation` capability plus an idempotent command observer for process keys and a configured credential sink for activation |
| Process-key discovery | PASS | host-agent starts/connects the exact slot+epoch worker, requests its canonical 32-byte X25519 key and returns it only with an image-matched slot observation |
| Ciphertext-only activation | PASS | encrypted onboarding material has a dedicated bounded field and never enters generic metadata; worker rotation output is copied/zeroed and forwarded through the existing single-writer NodeControl loop |
| Binding and replay defense | PASS | account runtime hash, slot, epoch, image, credential lease, proxy lease and command id are checked against the session-owned pending command; mismatches and a second commit are rejected before the Vault sink |
| Commit-before-ready | PASS | the host worker waits for the exact command ACK; only a canonical version id is accepted, while a rejected/malformed ACK fails activation and cannot produce a successful command result |
| Failure secrecy | PASS | Vault/KMS/sink errors collapse to `commit_rejected`; no sink error text, plaintext credential, proxy secret or ciphertext is copied into command metadata/error fields |
| Concurrency | PASS | heartbeats and other results continue while the Vault sink runs asynchronously; only the control-loop goroutine writes the gRPC stream and ACKs are routed by command id |
| Repeated race tests | PASS | control, host-agent and fake-provider suites passed `go test -race ... -count=10` |
| Full regression gates | PASS | execution-plane `make check` passed unit, full race, vet, Buf lint and generated drift; CCMAX `go test ./...` passed from its module root |

Task 5.5 remains unchecked. The transport and host-agent executor are now
library-complete, but CCMAX single/batch handlers and the durable provisioning
workflow still need to issue the two commands, persist the observed process key,
seal the activation package and expose status transitions. Tests used only
generated keys/ciphertexts, fake providers and loopback/in-process gRPC; no real
account, upstream, KMS, database, Docker daemon or remote server was accessed.

### Slice 17 — review hardening and orchestrator activation command builder (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Review finding | PASS | runtime health, exact epoch and exact image digest are now verified before either process-key use or encrypted credential delivery; a wrong-image runtime receives zero activation calls |
| Key-result binding | PASS | activation construction requires the issued key command id, healthy slot, exact slot/epoch/image and canonical X25519 key id/public key |
| Package integrity | PASS | onboarding material and the orchestrator rotation recipient key are encoded as one authenticated package and sealed to the observed worker process key with account hash, slot, epoch and both lease ids as AEAD context |
| Plaintext ownership | PASS | the builder consumes and erases the caller's onboarding input on success and failure; plaintext package and copied public-key buffers are erased after sealing |
| Command secrecy | PASS | only the bounded ciphertext bundle reaches the dedicated activation command field; command/package JSON formatting omits source secret material and generic metadata is unused |
| UI architecture decision | PASS (specification) | PRD/OpenSpec now defer UI until lifecycle state/DTO stability and select React + TypeScript + Vite + shadcn/ui, embedded by Go with pure white/dark neutral themes |
| Repeated race tests | PASS | service, host-agent and control suites passed `go test -race -count=10` |
| Full regression gates | PASS | execution-plane `make check` passed unit, full race, vet, Buf lint and generated drift; CCMAX `go test ./...` passed |

Task 5.5 remains unchecked. The next required boundary is a short-lived,
KMS-backed durable onboarding intent referenced by opaque id from the CCMAX
outbox, followed by provisioning command observation and single/batch handler
migration. No real account, upstream, KMS, database, Docker daemon or remote
server was accessed.

### Slice 18 — durable KMS onboarding intents and opaque CCMAX transition (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Intent envelope | PASS | temporary onboarding input is binary-encoded, immediately sealed with a per-intent KMS envelope and canonical AAD binding intent/account/desired generation/source/auth; caller-owned input and plaintext buffers are erased on every return path |
| Durable schema | PASS | `onboarding_intents` stores only ciphertext, encrypted DEK, nonce, exact AAD bytes, KMS metadata, owner/claim/intent expiry and consumed state; no plaintext or plaintext digest column exists |
| Idempotency and fencing | PASS | one opaque idempotency key returns the same intent before and after completion; account/generation/source/auth changes, another live owner, wrong generation and expired intent fail closed |
| Claim recovery | PASS | the same owner may replay within its claim, an expired claim may be taken over before intent expiry, and completion is idempotent after a lost response |
| Decrypt ordering | PASS | workflow rejects a wrong command id, node runtime binding, epoch, image or process public key before claiming/decrypting the intent |
| Completion ordering | PASS | intent is consumed only after exact activation success plus healthy slot/epoch/image; a valid result arriving just after command deadline still completes while intent claim/TTL remains authoritative |
| Maximum boundary | PASS | a full 1 MiB credential import plus authenticated binary framing is accepted without widening the public credential-material limit |
| CCMAX transaction | PASS | onboarding transition replaces all caller payload fields with only `onboarding_intent_id`, binds expected next generation and rolls back account/outbox mutation on stale generation or non-opaque/secret-looking identifiers |
| Regression gates | PASS | credential/onboarding/service/store suites passed repeated `-race -count=10`; execution-plane `make check`, CCMAX `go test ./...` and repository diff checks passed |

The authenticated CCMAX intake RPC and single/batch handler migration are not
part of this slice. Tests used generated material, fake KMS and in-memory or
sqlmock repositories only; no real account, upstream, KMS, MySQL, Docker daemon
or remote server was accessed.

### Slice 19 — durable provisioning observer and restart-safe controller (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Durable workflow | PASS | `onboarding_workflows` binds one intent owner and generation to exact node/slot/epoch/image, leases and distinct key/activation command ids; only the public process key and bounded secret-free error code are persisted |
| NodeControl handoff | PASS | both successful and failed key/activation results require the command observer; node/slot/epoch/image/command/key bindings are revalidated before an idempotent state update |
| State machine | PASS | `pending_key → key_dispatched → key_ready → activation_dispatched → activation_succeeded → completed` cannot skip credential-key validation, regress completed state or accept a conflicting duplicate result |
| Restart recovery | PASS | polling advances one durable step per call; stable command ids are re-dispatched only after configurable retry delay, mark-before/after-result races are idempotent and deadline expiry becomes `workflow_deadline_exceeded` |
| Ciphertext lifetime | PASS | durable process key reconstructs an authenticated activation package from the claimed intent; dispatcher receives only ciphertext and the controller erases its retained encrypted bundle after dispatch |
| Commit ordering | PASS | activation success is first observed durably, then intent completion and workflow completion are independently idempotent, covering a crash between the two database transitions |
| Review hardening | PASS | credential-envelope framing and credential business limits are separate; workflow creation reports duplicate idempotency correctly; dispatch/result retries never move `updated_at` backwards |
| Race and full gates | PASS | onboarding/service/control/store suites passed repeated `-race -count=10`; execution-plane unit/full-race/vet/Buf/generated gates, CCMAX full tests and `git diff --check` passed |

Task 5.5 remains unchecked. The observer/controller are library-complete but
not yet constructed by the orchestrator process, and CCMAX does not yet call an
authenticated onboarding intake or migrate its single/batch account handlers.
No real account, public upstream, cloud KMS, MySQL, Docker daemon or remote host
was accessed.

### Slice 20 — authenticated intake and single-account CCMAX bridge (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Distinct caller identity | PASS | the execution-plane CA issues a dedicated `spiffe://sub2api.execution/service/ccmax` client identity; node and service certificate parsers reject each other's identities |
| mTLS intake boundary | PASS (library) | the onboarding RPC requires TLS 1.3, a verified client chain and the exact configured CCMAX service identity before accepting temporary material |
| Request secrecy | PASS | Session Key/OAuth/Setup/API/Cookie bytes are handed directly to the KMS-backed intent Vault and explicitly erased on authorization, validation, repository and success paths; stable errors contain no credential text |
| Receipt contract | PASS | CCMAX accepts only an opaque, non-secret-looking intent id with exact account/generation/source/auth binding and a future expiry |
| Two-phase CCMAX bridge | PASS | the intent is created before the generation-fenced account/outbox transaction; the durable event payload is replaced by only `onboarding_intent_id`, while a transaction race leaves only a bounded expiring orphan intent |
| Migrated reauthorization | PASS | accounts already in `migrating`/`migrated` state submit Session Key asynchronously through intake and return `202 provisioning`; they never exchange or store AT/RT inside CCMAX |
| Explicit new-account path | PASS | `execution_onboarding=true` requires a fixed proxy, mTLS intake and `Idempotency-Key`; it creates an unschedulable `migrating/provisioning` account with empty local credentials before emitting the opaque intent transition |
| Legacy and batch isolation | PASS | the flag defaults off so the existing single-account path is unchanged; batch remains gated until worker-result identity projection can safely resolve email/deduplicate without CCMAX handling the Session Key |
| Regression gates | PASS | intake/PKI/service and CCMAX bridge/handler suites passed repeated race tests; execution-plane unit/full-race/vet/Buf/generated checks, CCMAX full tests and `git diff --check` passed |

The intake server, durable controller and credential sink are still libraries;
the orchestrator process has not yet assembled their production gRPC/DB/KMS
dependencies. Batch execution onboarding also remains gated on a durable worker
result projection. Tests used generated credentials, local TLS/bufconn servers,
fake KMS and mocked databases only; no real account, public upstream, cloud KMS,
project MySQL, Docker daemon or remote host was accessed.

### Slice 21 — orchestrator credential composition and bounded runtime (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Fail-closed composition | PASS | one constructor requires node, credential, intent and active-workflow repositories plus CA, KMS, durable rotation recipient and rotation authorizer before it creates either RPC service |
| Shared trust boundary | PASS | the same crypto service/Vaults, rotation recipient, command observer and credential sink are wired into NodeControl, intake, workflow builder, controller and runner; no partially configured credential RPC can register |
| Stable recipient recovery | PASS (primitive) | X25519 recipient restoration consumes and erases the caller's 32-byte private-key buffer and reproduces the same key id/public key; assembly no longer generates a new recipient on restart and destroys the injected key on failure/close |
| TLS process serving | PASS (library) | NodeControl and intake register once on one TLS 1.3 gRPC server with bounded 3 MiB messages; `VerifyClientCertIfGiven` permits certificate-free initial enrollment while certificate-free intake is denied by exact service identity authorization |
| Lifecycle coupling | PASS | RPC serving and workflow polling share cancellation; graceful stop has a bounded forced-stop fallback, runner exit terminates RPC and duplicate registration/registration-after-close fail without a gRPC panic |
| Bounded workflow scan | PASS | only nonterminal workflow ids are selected in deterministic `updated_at, workflow_id` order, at most 100 per pass by default, and each id receives at most one `Advance` call per pass |
| Failure isolation and fairness | PASS | one workflow error does not stop its batch; its secret-free `updated_at` retry marker moves it behind older work so a poison item cannot permanently starve later accounts |
| CCMAX ownership regression | PASS | an execution-owned account rejects the legacy Session Key endpoint with 409 before credential validation whenever authenticated intake is unavailable, for both empty and non-empty bodies |
| Race and full gates | PASS | recipient/component/RPC/runner and CCMAX ownership suites passed repeated race tests; execution-plane `make check`, CCMAX `go test ./...` and `git diff --check` passed |

Production construction described in this earlier slice is completed by Slices
22–26 below. Batch onboarding still requires the duplicate-aware lifecycle
switch; no real account, credential, public upstream, cloud KMS, project
database, Docker daemon or remote host was accessed by these tests.

### Slice 22 — protected production dependency loading (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Explicit enablement | PASS | production-only settings are required only behind `EXECUTION_ORCHESTRATOR_RUNTIME_ENABLED=true`; malformed boolean values fail closed and the existing health-only process remains the disabled default |
| MySQL transport policy | PASS | runtime DSNs require TCP, a database, `parseTime=true`, `loc=UTC` and verified TLS; plaintext, skip-verify and preferred/fallback TLS modes are rejected |
| Configuration secrecy | PASS | `String`, `GoString` and JSON serialization expose only whether a DSN is configured and never serialize its credentials |
| KMS identity | PASS | runtime configuration requires a bounded CVM role name and the Tencent KMS adapter still obtains credentials only from the instance-role provider, with no environment/file secret fallback |
| Service-key domain separation | PASS | the 32-byte rotation key uses `sub2api.service-key.v1` AAD and cannot be opened as an account credential or under another service purpose/version; sealing consumes and erases caller input |
| Protected recipient recovery | PASS | only a strict KMS envelope JSON in an absolute owner-only regular file is accepted; raw private-key env/files, symlinks, unknown JSON fields and wrong service-key purpose are rejected |
| PKI loading | PASS | CA/server private keys require owner-only files, certificate files reject group/other writes, final-component symlink swaps are fenced, and the server leaf must verify under the loaded CA for the configured server name and serverAuth usage |
| Regression gates | PASS | loader suites passed repeated race tests; execution-plane unit/full-race/vet/Buf/generated checks, CCMAX full tests and diff checks passed with generated keys, fake KMS and local temporary files |

This slice deliberately does not open the production database, contact Tencent
KMS or replace `cmd/orchestrator` serving. The durable rotation authorizer and
all-or-nothing runtime construction are still required before the production
enable flag may start the credential RPC path. No remote server was accessed.

### Slice 23 — fenced worker-result projection (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Secret-free extraction | PASS | worker reparses the exact normalized credential with unknown-field and mixed-auth rejection, then exposes only email, organization/account IDs, scope, subscription, tier and expiry; token/API-key values are absent from struct and JSON formats |
| Commit ordering | PASS | projection runs only after the rotation authorizer returns a committed Vault version and before the worker receives that version ACK; a projection failure rejects the ACK while a retry can reuse the version and idempotently repair the result row |
| Durable binding | PASS | `onboarding_results` is one-to-one with workflow/intent/credential lease and records exact account generation plus credential version; repository insertion revalidates runtime account hash, slot, epoch and both lease IDs against the locked workflow |
| Replay fencing | PASS | identical lease/version/metadata replay returns the original row and timestamp; changed proxy lease, version or projection is rejected rather than overwritten |
| Completion gating | PASS | result lookup joins the workflow and returns data only for `completed`; key-ready/activation-dispatched rows remain unavailable to CCMAX |
| mTLS query boundary | PASS | the existing exact CCMAX service identity authorizes result reads; request and response are bound to intent/account/generation and malformed or mismatched responses fail closed |
| Pending semantics | PASS | gRPC `FailedPrecondition` maps to a distinct CCMAX pending result while other RPC/validation failures remain generic intake errors |
| Race and full gates | PASS | worker/onboarding/store/service suites passed repeated `-race -count=10`; execution-plane unit/full-race/vet/Buf/generated checks, CCMAX full tests and `git diff --check` passed |

This slice does not switch the legacy batch Session Key handler. An email
collision between two execution-owned accounts must first use the unified
drain/archive lifecycle so the old slot and leases are fenced; direct database
overwrite would be unsafe. No real credential, upstream, cloud KMS, project
database, Docker daemon or remote server was accessed.

### Slice 24 — crash-safe credential rotation operation (2026-08-29)

| Check | Result | Evidence |
|---|---|---|
| Stable operation identity | PASS | the authenticated credential lease ID is passed from the worker commit sink to `Vault.RotateIdempotent`; malformed identities fail closed |
| Atomic persistence | PASS | `credential_versions`, active-version switch, superseded lease revocation and the unique `credential_version_operations` mapping share one MySQL transaction; mapping insertion failure rolls back before activation |
| Crash/retry convergence | PASS | an existing operation returns its exact account/auth/hint-bound version, while two independent Vault instances racing the same operation converge on one version and one durable mapping |
| Secret minimization | PASS | the operation table contains only opaque operation/version/account identifiers, auth type and timestamp; it stores neither worker material nor a plaintext-derived digest |
| Sink integration | PASS | worker commits no longer call unfenced `Rotate`; the sink supplies the credential lease ID to the idempotent Vault and preserves the higher-level authorizer callback fence |
| Race and full gates | PASS | credential/runtime-store/service suites passed repeated `-race -count=10`; execution-plane unit/full-race/vet/Buf/generated checks, CCMAX full tests and `git diff --check` passed |

This slice closes duplicate Vault-version creation after a lost acknowledgement
or an orchestrator crash. It does not by itself implement the production
rotation authorizer: durable material-digest comparison and an authoritative
current proxy-lease record are still required before the production credential
RPC may be enabled. No remote service or real credential was accessed.

### Slice 25 — durable rotation authorization and opaque proxy leases (2026-08-30)

| Check | Result | Evidence |
|---|---|---|
| Digest reservation | PASS | `credential_rotation_commits` reserves the authenticated canonical worker-frame SHA-256 under the unique credential lease before Vault runs; changed-material replay is rejected before execution validation or Vault |
| Runtime fencing | PASS | new/pending authorization locks the exact workflow and requires matching runtime account hash, slot, epoch, desired/actual generation, healthy active assignment, node and unrevoked/unexpired execution lease |
| Version completion | PASS | completion accepts only the version proven by the same credential lease in `credential_version_operations`; a missing, cross-account or changed version fails closed |
| Crash recovery | PASS | a pending digest first checks the atomic operation mapping and repairs completion even after command/execution expiry; if Vault never committed it must reacquire current execution/proxy authority, while a completed replay invokes neither proxy validation nor Vault |
| Proxy authority | PASS | `proxy_leases` stores only opaque ID/account/slot/epoch/timestamps, grants only against a current execution binding, rejects a second lease for the same epoch, and fails closed after revocation or execution expiry |
| Composition | PASS | production-style repositories automatically construct `DurableRotationAuthorizer`; explicit test authorizers remain injectable, and non-idempotent credential repositories no longer satisfy the component boundary |
| Race and full gates | PASS | credential/onboarding/runtime-store/service/control/host-agent suites passed repeated race tests; execution-plane unit/full-race/vet/Buf/generated checks, CCMAX full tests and `git diff --check` passed |

The runtime proxy lease proves that orchestrator accepted one opaque fixed-proxy
reservation for an exact account slot epoch; it intentionally does not copy or
validate the remote proxy endpoint or password. That upstream reservation sync
must grant/revoke this record through the trusted CCMAX outbox path. At this
slice boundary the production bootstrap was still disabled; Slice 26 supplies
the all-or-nothing construction without enabling or deploying it. No remote
service was accessed.

### Slice 26 — opt-in production orchestrator bootstrap (2026-08-30)

| Check | Result | Evidence |
|---|---|---|
| Explicit activation | PASS | generic health-only startup remains the default; only `EXECUTION_ORCHESTRATOR_RUNTIME_ENABLED=true` selects the production constructor, and invalid production config exits before runtime work |
| Database gate | PASS | startup opens the already-validated verified-TLS DSN, applies bounded pool settings, pings with a timeout and read-only checks every table required by the credential path; missing schema blocks KMS and listeners |
| Complete trust construction | PASS | one MySQL repository supplies node, idempotent Vault, intent, provisioning/result, rotation and proxy authorities; CVM-role KMS, protected PKI and KMS-restored recipient are all required before composition succeeds |
| Serving boundary | PASS | TLS 1.3 RPC and health start only after all dependencies are ready, share cancellation, and a component exit stops the other; recipient, listener and database are closed on every return path |
| Migration policy | PASS | startup performs no DDL and documents migrations as an explicit deployment step; schema verification is read-only |
| Secret-safe diagnostics | PASS | runtime errors expose only a bounded stage label and never wrap DSN/cloud/file errors; readiness logging contains addresses and service ID but no credential configuration |
| Race and full gates | PASS | schema/runtime tests passed repeated race runs with fake KMS, generated PKI, sqlmock and an in-memory listener; execution-plane unit/full-race/vet/Buf/generated checks, CCMAX full tests and `git diff --check` passed |

The process is now capable of serving the production credential control path,
but it remains operationally gated until migrations and protected files are
installed and the trusted CCMAX outbox grants/revokes opaque proxy leases. This
slice does not deploy or enable it on any host.

### Slice 27 — CCMAX onboarding completion and multi-source single-account intake (2026-08-30)

| Check | Result | Evidence |
|---|---|---|
| Runtime identity response | PASS | completed onboarding result now carries the stable slot and execution epoch read from the same locked workflow binding; repository scans and mTLS response validation reject missing or changed runtime identity |
| Generation-fenced completion | PASS | CCMAX scans only the current provisioning generation's opaque outbox intent and atomically revalidates account/intent/generation/slot/empty local credential before writing `migrated + ready + schedulable`, epoch, lifecycle audit and mode health |
| Pending and replay behavior | PASS | explicit pending leaves the account untouched; completed accounts leave the candidate set, stale slot/generation results cannot mutate state, and a second poll performs no result RPC or duplicate lifecycle write |
| Result error classification | PASS | only an absent/not-yet-terminal result returns the explicit pending sentinel and gRPC `FailedPrecondition`; canceled/deadline contexts, transient storage failures and corrupt persisted projections map to distinct non-pending gRPC codes so CCMAX cannot silently treat infrastructure failure as provisioning progress |
| Terminal result classification | PASS (execution-plane) | additive result fields explicitly distinguish succeeded, failed and expired; failure responses expose only fixed public codes/summaries, while workflow/provider/storage details stay internal; legacy responses remain wire-compatible through status zero |
| Duplicate identity boundary | PASS | an email collision leaves the new account unschedulable in `failed/duplicate_identity`, records only the conflicting account id and never overwrites either execution-owned credential; drain/archive remains a separate lifecycle operation |
| Mode safety | PASS | OAuth/Setup results enable both configured execution modes; ordinary API Key completion leaves `cli_native` unavailable and enables only `oauth_api` |
| Six material sources | PASS | explicit account creation accepts Session Key, OAuth code + PKCE verifier, Setup Token, API Key, Cookie and credential import through the mTLS intent; account credentials/source hint/outbox remain empty or opaque, and plaintext `credentials` JSON is rejected for execution onboarding |
| OAuth callback ownership | PASS | migrated accounts can create an OAuth PKCE session, but the callback code/verifier are sent to the worker intent and return `202 provisioning`; CCMAX performs no token exchange and stores neither value |
| Regression gates | PASS | execution-plane `make check` passed unit/full-race/vet/Buf/generated gates; CCMAX full unit, full race and vet passed after synchronized proto generation |

Task 5.5 remains open. Production still needs a coordinator that turns a
healthy assigned slot plus current intent into a durable workflow, grants the
opaque proxy lease from a trusted CCMAX reservation, publishes the fenced data
plane route and drains/archives duplicate batch results. No real account,
upstream, cloud KMS, project database, Docker daemon or remote server was
accessed by this slice.

### Slice 28 — durable CCMAX intake recovery and account-scoped resume (2026-08-30)

| Check | Result | Evidence |
|---|---|---|
| Durable receipt recovery | PASS | CCMAX persists the exact opaque receipt before the outbox transition; a lost Create response uses lookup-only Recover with explicit NotFound/Aborted/FailedPrecondition/Unavailable classification and no intent decryption |
| Attempt fencing | PASS | stable external keys are separated from random per-attempt intake keys; only exact expiry/commit-margin loss rotates through CAS, while late Recover/Create responses cannot mutate a newer attempt |
| Material ownership | PASS | Recover retains caller material, Create erases it before returning, and regression tests cover late and short-TTL receipts with/without a durable successor while proving no second Create of consumed material |
| Canonical create replay | PASS | version 1 SHA-256 binds only normalized non-secret typed account/group/proxy-selection/strategy fields; secrets, proxy text and free-form metadata are excluded, legacy/malformed/mismatched rows fail closed, and exact replay precedes mutable proxy availability checks |
| Account-scoped recovery | PASS | failed post-commit creates return `pending_account_id + resume_url`; status exposes neither key, resume rejects a client key and reuses the canonical submission after account/generation/proxy/migration/lifecycle/fingerprint checks outside the intake RPC transaction |
| Lifecycle safety | PASS | pending reauthorization may start only from legacy/ready/failed; provisioning/draining/destroying/archived/deleted and already-projected queued rows cannot be resumed or invoke intake |
| Bounded timing | PASS | CCMAX Create/Recover/Result calls default to a 30-second deadline with bounded configuration; production intent TTL defaults to 30 minutes and rejects values below five minutes |
| Regression gates | PASS | CCMAX full unit, targeted onboarding race, SQLite-migration and vet gates passed; execution-plane full unit/race/vet/Buf/generated `make check` passed after synchronized protobuf generation |

Task 5.5 remains open. The next safe slice is trusted CCMAX proxy-reservation
grant/revoke, followed by an atomic starter that locks the current intent,
healthy slot/assignment, execution lease and trusted proxy grant before creating
one workflow and proxy lease. Route publication and duplicate drain/archive
remain later slices. No real credential, upstream, Docker daemon, cloud service,
project database or remote host was accessed.

### Slice 29 — trusted proxy authority, immutable occupancy and generation handoff (2026-08-30)

| Check | Result | Evidence |
|---|---|---|
| CCMAX authority | PASS | `runtime_proxy_reservations` durably binds opaque reservation/grant/revoke IDs to one account, desired generation, canonical positive-decimal proxy row ID and monotonic binding revision; exact grant/revoke replay is fenced by database uniqueness |
| Transaction ordering | PASS | successor onboarding locks account/proxy and commits `revoke(old) -> generation CAS -> grant(new) -> onboarding event`; ordinary runtime transitions do not revoke the retained reservation |
| Immutable occupancy | PASS | proxy/pool identity mutations, delete/restore/quarantine, allocator availability and pending nonlegacy ownership all check the same runtime authority; a cross-account active reservation cannot be selected or granted twice |
| Probe-before-use fence | PASS | single and batch proxy tests acquire Serializable proxy authority locks before any network call, retain them through result persistence, and return 409 with zero probe calls when any target is runtime-owned; SQLite uses a no-op write fence and MySQL row locks |
| Legacy lifecycle safety | PASS | old archive/delete/restore paths reject the entire matched batch for runtime-managed accounts with zero partial writes, preserving the PRD's retained-reservation semantics until the unified drain/destroy lifecycle exists |
| Execution projection | PASS | strict grant/revoke decoding accepts only the exact three-field payload and canonical numeric proxy binding; assignment desired generation is persisted separately and same-image generation drift now converges through drain/destroy/release |
| Temporal fencing | PASS | proxy-lease replay uses UTC MySQL `DATETIME(6)` precision consistently in SQL/memory, rejects future proxy/execution lease creation, and validates current reservation/execution authority at the checked instant |
| Regression gates | PASS | CCMAX full unit, SQLite migration-tag, vet and full race passed; execution-plane targeted authority/reconcile suites passed unit and race; repository diff checks passed |

The real endpoint and proxy credentials remain CCMAX/host-agent owned and never
enter the execution projection. This slice intentionally does not implement the
unified “archive while retaining reservation” or explicit “archive and release”
workflow. No external proxy, account, upstream, project database or remote host
was accessed.

### Slice 30 — atomic healthy-slot onboarding starter and pre-decrypt reauthorization (2026-08-30)

| Check | Result | Evidence |
|---|---|---|
| Atomic starter | PASS (library) | one Read Committed transaction locks metadata-only pending intent state, ready slot, same-generation/image fresh healthy assignment, live execution lease and trusted reservation before inserting the exact proxy lease and workflow |
| Secret boundary | PASS | the starter query selects no ciphertext/encrypted DEK and does not claim or decrypt the intent; all generated identities are deterministic opaque values and persisted records contain no proxy endpoint or credential |
| Exact replay | PASS | one `intent_id` may own at most one workflow; an exact replay returns the original workflow/proxy binding before mutable health checks, while changed slot/reservation/revision identity fails closed |
| Schema gate | PASS | migration 010 adds the one-workflow-per-intent unique index; startup verifies that the named index is unique and covers exactly `intent_id`, so missing or malformed partial migration state cannot serve |
| Bypass removal | PASS | the production MySQL repository no longer exposes direct `CreateProvisioning`; workflow creation can no longer bypass healthy assignment, execution lease, trusted reservation or proxy-lease insertion |
| Activation reauthorization | PASS | current proxy/execution authority is checked immediately before and after intent claim/KMS open; a revoke before claim causes zero opens, while a revoke racing the open erases the input and prevents worker dispatch |
| Review hardening | PASS | canonical proxy IDs, same-image generation drift, nanosecond-to-microsecond replay parity, future-created lease rejection and exact index-shape verification close all independent P1/P2 findings found in this review |
| Full regression gates | PASS | `execution-plane make check` passed unit, full race, vet, Buf lint and generated-code drift; CCMAX full unit, SQLite migration-tag, vet and full race passed; `git diff --check` passed |

This is not the production coordinator yet. CCMAX outbox routing, deterministic
intent-to-slot selection, starter polling/claim scheduling, data-plane route
publication and duplicate-identity drain/archive batches remain open under task
5.5c. Migration 010 safely fails when historical duplicate-intent workflows
exist, but deployment still needs a read-only preflight/report and an operator
resolution runbook. Concurrency has been proven with sqlmock, in-memory race
tests and repository invariants, not a real multi-connection MySQL integration
test. No schema was applied and no Docker daemon, cloud KMS, real credential,
external upstream, project database or remote host was touched.
