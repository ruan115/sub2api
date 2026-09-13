# Fake transport slice

Status: implementation plan recorded before code changes, 2026-09-13; the
corresponding fake-only implementation and offline tests are now present. This slice
uses Bun 1.3.9 and synthetic fixtures only. It does not turn the recovered bundle
into a service or enable the production execution plane.

## Files and dependency direction

```text
src/runtime/turn/
  types.ts             TurnEngine / TurnHandle / metadata ports
  errors.ts            public error classification without input disclosure
  cancellation.ts      abortable waits and cleanup
  request.ts           bounded JSON request interpretation
  fake.ts              deterministic, bounded fake-only driver
src/transport/shared/
  body.ts              bounded request/response byte collection
  errors.ts            HTTP-compatible error envelopes
src/transport/http/
  handler.ts           /v1/messages and fake health handler
  response.ts          pull-driven response with cancellation
  local-access.ts      exact bound Host/Origin gate shared by HTTP and WS upgrade
src/transport/websocket/
  controls.ts          bounded pending protocol metadata
  writer.ts            one drain wait, no outgoing queue
  session.ts           one in-flight request, cancellation and reuse
src/app/
  fake.ts              pure assembly; imports do not open sockets
  serve.ts             explicit loopback-only Bun listener factory
  shutdown.ts          native-first stop, dual cleanup and failure deadline
```

Tests live beside the corresponding implementation. Test helpers and synthetic
fixtures may live under `test/`; none contain captured requests or identities.
Transport imports the turn port. The fake driver has no filesystem, environment,
CLI, credential, proxy URL or upstream/network dependency. App assembly only
constructs the fake driver, and any listener requires an explicit factory call.

## Turn interface

`TurnEngine.start(request, signal)` returns a handle with response-start metadata,
a pull-driven async byte iterator, an idempotent `cancel()` and a completion
promise. Request data includes a validated non-empty messages array, resolved
model, stream flag and optional protocol-control metadata. The driver produces
fixed synthetic text, never echoes message content, and exposes bounded active
turn counts for tests. Fake scenarios include success, a non-2xx response and a
failure after the first chunk. Capacity exhaustion rejects instead of queueing.

HTTP returns a response as soon as start metadata exists. Streaming reads one
iterator chunk per pull with highWaterMark zero. Non-streaming and error bodies
are collected under a response-byte limit. Request bodies are counted while
reading, with a default 1 MiB cap. Aborting requests or cancelling response
readers cancels the handle, including a handle not yet consumed.

WebSocket sessions reuse the existing one-byte codec. One REQUEST may be active;
a second receives an ERROR envelope with HTTP-like status 409. CANCEL has no
acknowledgement/END and suppresses later output for that turn. The connection
becomes reusable after cleanup. Successful turns emit RESPONSE_START, CHUNK(s),
END. A failure before start is an ERROR envelope; an iterator failure after
start closes with 1011, matching the reference's truncated-stream behavior.

The sender has a single abortable drain wait and no growing message queue. The
explicit Bun listener also has a 128 KiB backpressure limit with close-on-limit.
If
another write arrives while the peer is backpressured, the session closes with
1013 rather than accumulating control/error frames. Pending beta controls have
explicit byte/token caps. These are deliberate fake-service resource policies.

## Evidence and explicit limits

Only static reads of the private `isthmus.readable.mjs` informed this design:

- 30262–30288: tags, frame bytes, start/error JSON envelope field names.
- 30350–30394: control dispatch, busy 409, pending-control consumption.
- 30410–30513: chunk passthrough, END, cancellation and failure-close semantics.
- 30519–30544: HTTP routes, WS upgrade and 64 MiB application-message ceiling.
- 29942–29966: pull-driven HTTP stream, highWaterMark zero and cancellation.
- 23394–23410: body parsing, messages-array validation and model resolution.
- 23625–23644: beta token charset and usage-limit validation.

Fake-only differences: default model is `fake-model`; response text, IDs and usage
are synthetic; requests have an explicit cap; output/control queues are bounded;
no credentials are read or accepted as configuration; `/` exposes fake mode only.
The public entry point is loopback-only and marks application-handler responses as fake. All
other listener hosts are rejected. There is no CLI launcher or implicit startup.

Additional local-demo safety policies, not restored production authentication:
HTTP and WS upgrade share an exact bound Host/port check and require same-origin
HTTP Origin when present. DNS-rebind/foreign hosts and null/cross-origin requests
are rejected before any upgrade or turn. Origin-less CLI requests are allowed.
HTTP bodies must use application/json and finish reading within a cancellable
10-second deadline. HTTP/WS idle timeouts are 15/30 seconds. No CORS allowance is
added. These stricter demo boundaries require separate compatibility decisions.
Native ingress caps are also fake-only: HTTP maxRequestBodySize equals the
configured request cap, and WS maxPayloadLength is min(64 MiB, max(request cap,
8 KiB) + 1 tag byte). The codec's 64 MiB boundary is unchanged. A native rejection
can precede the application handler and omit its marker/error envelope.

This slice does not implement an SSE-to-Message parser: the fake engine supplies
JSON chunks for non-streaming requests and SSE chunks for streaming requests.
It does not implement gRPC, upstream execution, CLI/PTY, OAuth, MCP, request/body
rewriting, identity compatibility, telemetry modification, billing, production
migration or full WebSocket keepalive/compression parity. Beta/usage metadata is
accepted for framing tests but does not modify an upstream request. Live
compatibility remains unverified.

## Verification

Offline tests cover early first chunk, demand-driven production, UTF-8 byte
splits, malformed/body-limit requests, non-2xx and truncated-stream errors,
cancellation before/during reads, WS busy/reuse, drain cancellation and bounded
control state. They exercise fake peers rather than opening network listeners.
App factory tests verify import/assembly side effects, fake markers, loopback
restrictions and cleanup, including shutdown during an unfinished request-body
read. Cancellation signals are composed without reconstructing Request streams:
the Bun 1.3.9 streamed Request path is exercised directly. No test imports or
executes recovered JS/ELF/scripts.

## Discovered native shutdown limitation

Local Bun 1.3.9 loopback testing uncovered a distinction not represented by
offline peer mocks: native stop must start before runtime graceful-close calls
to avoid a client-close race. That ordering is now explicit and unit-tested.
A separate server-initiated close can permanently retain Bun's WebSocket count,
even after an independent client has observed code 1011. Event-loop barriers,
terminate and client-process separation did not resolve this in bounded probes.

The listener still awaits native stop and runtime cleanup. Its default 3000 ms
shutdown deadline rejects with FAKE_SHUTDOWN_TIMEOUT and stats.shutdown=failed;
it does not claim termination, change the wire close code, exit the host process
or upgrade Bun. This limits caller wait time but does not repair the native
handle. Full D4 shutdown acceptance remains incomplete. See the related
[upstream report](https://github.com/oven-sh/bun/issues/36223); its reported
version/status is not evidence that pinned Bun 1.3.9 is fixed.
