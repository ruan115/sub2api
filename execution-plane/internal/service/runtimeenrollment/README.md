# Production runtime enrollment dependencies

This module is opt-in via `EXECUTION_RUNTIME_ENROLLMENT_ENABLED=true`, in addition
to the existing orchestrator runtime flag. It requires an explicit
`EXECUTION_LEASE_REDIS_ADDR` and never reuses route Redis implicitly. Only clean
private/loopback numeric addresses or DNS hostnames with a numeric port are
accepted; deployment DNS and network ACLs remain operator responsibilities.
`EXECUTION_RUNTIME_ENROLLMENT_TIMEOUT` defaults to 2s and cannot exceed 5s.

The existing runtime SQL schema must already be installed and verified. Receipts
and assignment bindings use that database. Startup performs a bounded Redis PING,
not writes or migrations. The Redis connection belongs to this module; the SQL
connection belongs to the orchestrator. Errors do not include backend responses.

The lease namespace is fixed at `config.RuntimeLeaseKeyPrefix`
(`execution:lease:v1:`). **The production execution-lease writer is still missing.**
Connecting successfully does not create a lease or authorize a worker. Until the
authoritative writer supplies the matching current lease, enrollment fails closed.
There is no memory fallback, automatic grant, certificate renewal or readiness
publication here.
