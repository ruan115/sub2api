# Command-bound diagnostic tickets

This is an opt-in library path over the existing TLS NodeControl stream, not a
production ticket service or permission to execute model requests.

| Command | Permitted scope | Generation source |
| --- | --- | --- |
| INSPECT SlotCommand | `health` | Canonical positive `metadata.desired_generation` |
| CredentialKeyCommand | `credential_key` | Typed field populated from `SecureOnboardingPlan.DesiredGeneration` |
| Other/legacy command without generation | None | No fallback for opted-in command contexts |

The host opts in with `EnableProbeTickets` and the `probe_tickets` capability.
The control server separately requires `ProbeTicketConfig` with a read-only
ProbeBinding repository, independent lease validator and control-owned signer.
Neither production bootstrap nor its default configuration is changed.

Requests supply command/scope and a fresh correlation request ID, never an
account, node, epoch, generation or URL. The server derives identity from the
current authenticated session and exact pending command instance, requires that
the command has reached the stream-send handoff, and brackets issuance with
authority/session/lease checks. A reserved or merely queued command cannot
obtain a ticket. Each eligible command instance has at most one attempt;
failure or loss requires a new command, not a cached token or new request ID.

The default ticket TTL is 5 seconds (at most 10), further rounded down to the
original command/durable-lease/node-freshness/certificate ceilings. Authority
checks default to 2 seconds (at most 5). A later heartbeat cannot extend the
attempt. Queued responses are rechecked on dequeue. The host bridge bounds
pending requests/queue at 64, waits at most 10 seconds and no later than the
command deadline, and discards valid late correlation IDs. It never starts a
second stream sender or supplies a signing private key to the worker/host.

`RequestProbeTicket` only works within a currently running opted-in command.
`Runtime.issue` uses that source only after matching its complete runtime
identity; denied/unsupported scopes do not fall back to the legacy TicketSource.
Tokens are bearer secrets: do not log or persist request responses. The host
parses shape/correlation; the actual worker Guard verifies the signature and
consumes the nonce once.

Boundaries that remain open:

- `Validate` does not expose Redis remaining TTL. A sent diagnostic ticket can
  retain read-only authority until its configured expiry after revocation; this
  is not immediate revocation, a reusable readiness proof or renewal permission.
- ProbeBinding needs an existing provider reference. This does not complete new
  instance onboarding, the B3 existing-only runtime registry, or production
  provider/worker diagnostic assembly. The default INSPECT executor still checks
  the provider; the combined test uses an explicit synthetic diagnostic executor.
- No activation/business tickets, version-bound execution authority, lease
  writes, gateway/CLI deployment or production switches are added here.

The local integration exercises the actual TLS control server/client and worker
Health/public-key RPCs with ephemeral test certificates/signing keys and Memory
authority/leases. It rejects scope escalation/replay and asserts zero model
execution and no credential activation. It does not test production accounts,
databases, Redis, proxy connectivity, Docker or VMs.
