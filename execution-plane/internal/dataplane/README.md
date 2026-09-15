# Private CCMAX execution data plane (DP1)

This package implements the existing `execution.v1.ExecutionDataPlaneService`
wire contract as an injectable host-side boundary. It does not open a listener
by default, provision a runtime, read production configuration, mint tickets,
or connect the CCMAX gateway. Browser code must never call worker/Docker APIs.

```
CCMAX service client -- TLS 1.3, ccmax service certificate --> data plane
    -- active account/slot/node/epoch/generation + lease --> existing runtime
    -- one-time scoped ticket, opaque account identity --> worker RPC
```

## Composition and responsibilities

- `NewGRPCServer(Config, *tls.Config)` requires TLS 1.3, client CA and
  `RequireAndVerifyClientCert`; it returns an unstarted server. Every method also
  verifies the transport's verified leaf and exact `ccmax` SPIFFE service ID.
- `FencedResolver` requires a **trusted, internally consistent** `SnapshotSource`,
  a current execution `LeaseValidator`, and `RuntimeLookup`. A Redis route cache
  lacks account authority and is not a substitute for the snapshot source.
  No production snapshot projection or repository access is wired in this slice.
- `RuntimeLookup` only returns an existing runtime, never `Controller.Start` or
  user-selected addresses. Resolve checks the complete binding before lookup and
  again afterwards. Snapshots older than 45 seconds (configurable only lower),
  including those that age out during lease I/O, are rejected.
- `hostagent.Runtime.OpenExecution` / `CountTokensRequest` copy request fields,
  replace the external account ID with the existing opaque runtime identity, and
  request a one-time scoped ticket from the injected control-plane TicketSource.
  Host-agent never creates a signing key. Integration fixtures use ephemeral keys.
- Worker checks account/slot/epoch a second time and requires a nonzero route
  generation. **Tickets and worker identity do not contain authoritative route
  generation**; that check belongs to the host-side snapshot resolver.

## Streaming boundary

First frame must be Begin. Subsequent frames may contain ToolResult or Cancel;
half-close continues receiving responses. Relay sends individual responses with
bounded queues and HTTP/2 backpressure, not a whole-stream response slice.
Cancel/context timeout closes only the current worker RPC, not its shared
connection or the runtime instance. Completion/error closes the worker stream
immediately; extra events after a terminal event are discarded, not verified.

Authority checks run during opening and active execution at most one second
apart, with bounded check deadlines. Revocation or unavailable storage cancels
the request. This is bounded-period detection, not instantaneous atomic cut-off;
the existing protected-egress lease fence remains independently necessary.

Requests permit only the existing six header names. Responses currently permit
only `content-type` and `x-request-id`, matching the existing worker executor;
unexpected headers fail closed. Future rate-limit headers need explicit tests
and contract expansion. Runtime errors use fixed summaries; no retry into legacy
or plaintext credentials exists here. `ListModels` is explicitly Unimplemented.

Maximum relay message is 32 MiB; header map 32 KiB; session key 512 bytes.
The subsequent HTTP-worker acceptance slice now sends real upstream SSE in
32 KiB chunks, with bounded usage observation and terminal checks; it no longer
collects the whole SSE response under the former 2 MiB cap. Worker requests,
nonstream/error bodies and its per-message gRPC limit remain independently
bounded near 2 MiB. See [HTTP relay scope](../worker/upstream/README.md).
Real CLI/MCP, production composition and end-to-end size parity remain open.

## Repeatable local check

From the repository root:

```sh
make -C execution-plane dataplane-check
```

Tests use synthetic dependencies and disposable loopback listeners. The two-hop
tests exercise actual TLS handshakes, signed worker tickets, first-chunk-before-
completion, tools, half-close, cancellation, lease loss, generation replacement,
and 4 MiB cumulative output in 64 KiB chunks. These do not prove Docker network
isolation, real model behavior, production signing, or UI-to-gateway integration.

Worker RPC now rejects missing slot/epoch/generation. Upgrade its callers and
worker together after the deployment gate; do not deploy this worker alone with
an older host client. Deprecated collecting helpers now require explicit route
generation and are retained only for local Docker fixtures, not the data plane.
