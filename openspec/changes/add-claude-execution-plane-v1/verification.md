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
| Repeated race tests | PASS | credential, worker, service, host-agent and provider suites passed `-race -count=10`; targeted package vet and diff checks passed |

Task 5.5 remains unchecked. The NodeControl protocol still needs an additive
orchestrator/host-agent key-discovery and encrypted activation command, and the
CCMAX single/batch handlers have not migrated. Tests used fake providers,
generated keys/secrets and in-process gRPC only; no Docker daemon, public
upstream, project database or remote server was accessed.
