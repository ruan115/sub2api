# Session-bound execution authority (B1)

This is a control-plane library, not a host-agent listener or a new source of
account/business authority. It reads existing execution state and owns no
credential, CA key, ticket key, runtime provisioning or route cache.

## Modules

- `runtime/store/execution_binding.go`: one consistent MySQL JOIN across slot,
  active assignment, node and durable lease; MemoryRepository uses one read
  lock. Exact identity/session/image comparisons, generation/state/freshness
  and lease bounds are required. Returned observation time is never renewed.
- `control/session.go`: the same accepted, authenticated TLS 1.3 control stream
  must remain live on this orchestrator. Its node certificate and durable
  session are checked without holding the session-map lock during storage I/O.
- `snapshot.go`: combines these read-only gates, rechecks time after I/O and
  returns `dataplane.Snapshot`. FencedResolver still needs independent Redis
  lease validation and an existing-runtime-only lookup. It compares session ID
  as well as assignment/ref/owner across lookup.

Migration **013** adds nullable `observed_control_session_id`; historical values
stay NULL. New NodeControl results take their session ID from the server, never
from a node message. Assignment proof and current connected node session are
serialized in one transaction. Failed/unhealthy results clear proof; legacy
unscoped observation writes also clear it. Fresh successful healthy results
must match the issued image, the locked durable assignment image and command
deadline, including observations without provisioning jobs. Observer I/O cannot refresh
the receipt timestamp; its command deadline also bounds storage work.

## Deliberate fail-closed behavior

- Missing/stale/unknown-session records are unavailable, not automatically
  recovered. A new Hello or heartbeat does not confirm old assignments.
- No observation is refreshed by heartbeat, query time or an unavailable
  dependency. Without a new controlled runtime confirmation, it expires in at
  most 45 seconds. B2a adds an opt-in `runtimeprobe` INSPECT runner; production
  wiring, worker health/version proof and renewal remain later B2/B4 work.
- An orchestrator without the node's current live stream denies the request.
  Multi-orchestrator RPC routing/ownership is not implemented here.
- `Snapshot.Ready` means a current scheduling candidate, **not** that mode,
  active credential/proxy version or business authorization are approved.
- Lookup's before/after session check does not pin an already-open stream to
  its original session. Active stream cancellation and registry eviction on
  reconnect remain B3/B4 gates; a fast disconnect/reconfirm between periodic
  checks must not be described as instantaneous revocation.

## Verification boundary

Tests include actual local TLS NodeControl enrollment/stream/command results,
MemoryRepository and this projection; independent FencedResolver composition;
SQL query/transaction contracts; cancellation/deadline and reconnect races.
They do **not** prove actual MySQL migration/locking, Redis, Docker, upstream
model behavior or gateway/worker full-chain compatibility.

Migration scripts are committed but never applied automatically. In particular,
the new result transaction locks node before assignment; existing release paths
can use the opposite order. MySQL deadlock/retry behavior needs isolated real
database verification before production approval. A transaction failure is not
permission to serve from stale proof.

No entry point instantiates this source yet. Production RPC/signing, host-agent
assembly, runtime registry, worker modes and final load/soak/canary gates remain
open in the [acceptance plan](../../../docs/plans/ccmax-execution-acceptance-v2.md).
