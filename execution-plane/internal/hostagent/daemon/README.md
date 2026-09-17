# Lifecycle-only host-agent entry

`cmd/host-agent` injects `SelectRunner` into the shared service bootstrap. The
shared service does not import node implementations. Nothing in this package
enables business execution, onboarding, credential activation or deployment.

## Files and boundaries

- `entry.go`: pure opt-in selection, no fallback on invalid configuration.
- `config.go`: bounded explicit host configuration, fixed sandbox policy.
- `identity.go`: existing node identity loading and independent control TLS.
- `composition.go`: one Docker provider, RPC enrollment, authenticated existing
  START composition and the real control client. No alternate raw START.
- `executor.go`: sealed admission, context-aware serialization and bounded wait
  for queued/running commands, including commands from prior control sessions.
- `run.go`: startup ownership, certificate expiry and shutdown supervision.
- `health.go`: process liveness; business readiness remains false / HTTP 503.

All corresponding tests use synthetic identities and loopback fixtures. The
Unix HTTP fixture is **not a Docker daemon**. Prior real Docker evidence and
worker TLS tests are separate; neither proves this full binary on a Linux host.

## Configuration contract (not deployment instructions)

`EXECUTION_HOST_AGENT_RUNTIME_ENABLED` defaults off; only the exact `true`
opts in (`false` or empty remains off). Off reads no other host runtime fields,
loads no node files and dials no dependency. The prior health-only default is
unchanged; its readiness must not be interpreted as a configured execution host.

Enabled mode requires these `EXECUTION_HOST_AGENT_` fields:

| Suffix | Meaning |
| --- | --- |
| `CONTROL_ADDRESS` | Canonical private/loopback literal IP and port; no DNS dialing |
| `CONTROL_SERVER_NAME` | DNS verification name in the configured control certificate |
| `DOCKER_SOCKET` | Explicit clean absolute Unix socket path; no Docker context/env discovery |
| `TRUST_FILE` | One trusted CA PEM |
| `NODE_CERT_FILE` | One pre-issued exact-node SPIFFE client certificate |
| `NODE_KEY_FILE` | Matching P-256 private key, owner-only regular file |
| `TICKET_PUBLIC_KEY` | Canonical unpadded base64 Ed25519 verification key, never a signer |
| `UPSTREAM_BASE_URL` | Credential-free HTTPS origin |

Optional resource limits default to CPU 500m, memory 512 MiB, 128 PIDs and
64 MiB tmpfs (`CPU_MILLI`, `MEMORY_BYTES`, `PIDS`, `TMPFS_BYTES`). Security is
fixed: UID 1000, readonly root, no-new-privileges, all caps dropped, builtin
seccomp, docker-default AppArmor. Configured capacity is a ceiling, not live
free-host capacity discovery. The provider still needs its outstanding swap
and actual kernel/egress isolation acceptance before production use.

`EGRESS_PROXY_URL` defaults to `http://host-agent.execution.internal:8094`;
only the fixed internal hostname with canonical port is allowed. No proxy
service is started here. `RUNTIME_PORT` defaults to 8093. `READY_TIMEOUT`,
`STARTUP_TIMEOUT`, `SHUTDOWN_TIMEOUT` default to 30s, 5s, 10s, capped at 60s,
30s, 30s respectively. Health configuration still uses `EXECUTION_LISTEN_ADDRESS`
and `EXECUTION_NODE_ID`; enabled mode additionally requires a literal loopback
health listener and the runtime identity's lowercase node-id grammar.

Certificate paths must be physical paths: symlinks in any component, hardlinks,
aliased files, unsafe ownership/permissions, trailing PEM and file replacement
are rejected. The key's immediate directory cannot be group/world writable.
No files are created, rewritten, automatically enrolled or rotated. The run
ends at the earlier CA/node expiry, including already-established connections.
Errors and formatting of configuration/identity never echo values or material.

## Explicit non-readiness and shutdown

Hello carries `execution_mode=lifecycle-only` and `lifecycle_only`, not docker,
image, activation or probe-ticket capabilities; no data-plane endpoint is
advertised. Placement excludes either marker even for unconstrained/sticky
requests. Authenticated **explicit lifecycle commands** remain possible; this
is not permission to run this binary against production or adopt business slots.

Shutdown seals new/queued commands, cancels control operations, waits for tracked
calls, and then closes owned transports. A context-ignoring operation cannot be
forcibly interrupted by Go: deadline failure returns `ErrShutdown` and the binary
exits nonzero; it never reports a clean drain or deletes/stops containers as
cleanup. There is no business runtime registry, lease writer or active lease
revocation enforcement for already-running business flows in this slice.

The independent orchestrator enrollment option lives in
`internal/service/runtimeenrollment/`. It requires real SQL bindings/receipts
and a separate Redis lease validator. Empty Redis does not authorize issuance.
