# Isthmus runtime protocol foundation and fake transports

This private module contains the recovered `isthmus.v1.Messages` contract and a
new, independently written TypeScript WebSocket application-frame codec, plus
a deterministic fake turn engine and HTTP/WebSocket adapters. It remains a
recovery foundation, **not the recovered production isthmus service**. Every
application-handler response is marked `x-isthmus-runtime: fake`. There are no
third-party dependencies and no install step.

The separate [image module](image/README.md) provides a base-only Dockerfile
template and offline reviewed-input staging. It is not a built image or a
production service entrypoint; app/Bun/CLI integration and cold-start gates
remain open.

## Run the offline tests

The tested and pinned toolchain is Bun 1.3.9. From this directory:

```sh
bun test
```

Or from the repository root:

```sh
bun --cwd execution-plane/isthmus-runtime test
```

The default `bun test` suite runs only newly written code and synthetic bytes. The proto integrity test
reads the checked-in contract and its provenance; the original private recovery
directory is not needed. Tests create no listener, child process, network
request, account, credential or database. Boundary tests allocate real buffers
of approximately 64 MiB each to check the inclusive message limit.

Optional actual local transport smoke (also included by `make -C recovery check-demo`):

```sh
bun run test/loopback.smoke.ts
```

This separate entry opens an ephemeral literal-loopback listener, uses synthetic
HTTP/WS clients to verify early chunks, cancellation, framing and Host/Origin
rejection, then awaits shutdown. It never contacts the original server or a model.
It is not a live reference-compatibility test.

## Ownership

- `contracts/grpc/messages.proto`: the single canonical proto, preserved
  byte-for-byte, including its original comments. No client/server is generated.
- `contracts/grpc/provenance.json`: source location, extracted-bundle line range,
  exact size and SHA-256. This is provenance, not proof of live compatibility.
- `src/protocol/websocket/tags.ts`: known direction and wire-tag definitions.
- `src/protocol/websocket/validation.ts`: tag, direction and message-size guards.
- `src/protocol/websocket/codec.ts`: byte encoding/decoding only.
- `src/protocol/websocket/errors.ts`: payload-free framing error codes.
- `src/runtime/turn/`: fake-only turn port, deterministic driver and cancellation.
- `src/transport/http/`: bounded `/v1/messages` request parsing, pull-driven SSE
  output and bounded non-streaming/error collection.
- `src/transport/websocket/`: pending controls, one-drain writer and single-turn
  session adapter with busy, cancel, reuse and error handling.
- `src/app/fake.ts`: pure assembly, finite session capacity and shutdown.
- `src/app/serve.ts`: explicit opt-in Bun listener restricted to literal loopback.
- `src/app/shutdown.ts`: native-first stop, dual cleanup and explicit failure deadline.
- `docs/fake-transport-slice.md`: pre-implementation layout, static evidence,
  fake-only resource policies and compatibility exclusions.

The module follows `recovery/docs/architecture.md`: protocol code does not import
transport, runtime, credentials or application assembly. The recovered bundle
is not copied, imported or executed here.

## Application frame contract

The codec operates on a **complete WebSocket message body**, after WebSocket
reassembly/decompression. It does not implement RFC 6455 masking, opcodes,
fragmentation or compression. Encoding is `one byte of application tag + payload
bytes`; there is no length prefix or base64 conversion.

| Direction | Tags |
| --- | --- |
| `client-to-server` | REQUEST `0x01`, CANCEL `0x02`, BETA `0x03`, BETA_REMOVE `0x04`, BETA_REPLACE `0x05`, USAGE_LIMIT `0x06` |
| `server-to-client` | RESPONSE_START `0x10`, CHUNK `0x11`, END `0x12`, KEEPALIVE `0x13`, ERROR `0x14` |

The public entry point is `src/protocol/websocket/index.ts`:

```ts
import {
  CLIENT_FRAME_TAGS,
  decodeFrame,
  encodeFrame,
} from "./src/protocol/websocket/index";

const bytes = encodeFrame(
  "client-to-server",
  CLIENT_FRAME_TAGS.REQUEST,
  new TextEncoder().encode('{"synthetic":true}'),
);
const frame = decodeFrame("client-to-server", bytes);
```

`encodeFrame` copies the payload. `decodeFrame` returns a borrowed `Uint8Array`
view over the input buffer, matching the recovered `subarray(1)` behavior.
Callers must retain the buffer and copy the view if they require isolation from
later mutation. Encoding/decoding honors a supplied view's offset and length.

Both operations reject unknown/wrong-direction tags and messages over
67,108,864 bytes (64 MiB **including the tag**). Maximum payload size is therefore
67,108,863 bytes. Decoding rejects a zero-byte message. All known tags accept an
empty payload at the framing layer; an empty REQUEST is not evidence of a valid
JSON request. Payload validation belongs to the transport/turn layer.

Nonempty control payloads are also preserved. The original CANCEL handler ignores
its payload; this codec does not invent an empty-only restriction. BETA token
validation, the significance of an empty BETA_REPLACE, USAGE_LIMIT handling,
status/header JSON, stream completion and request concurrency are not interpreted
by the codec; the fake transport adapter handles them separately. The optional
Bun listener converts text messages to UTF-8 bytes before dispatch.

## Use the fake application without a listener

```ts
import { createFakeRuntime } from "./src/app/fake";

const runtime = createFakeRuntime({ delayMs: 5, chunkBytes: 128 });
try {
  const response = await runtime.handleHttp(new Request("http://fake.invalid/v1/messages", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ messages: [{ role: "user", content: "synthetic" }], stream: true }),
  }));
  // Consume response.body incrementally; no fetch or network request occurs.
  await response.body?.cancel();
} finally {
  await runtime.close();
}
```

`createFakeRuntime` only constructs `FakeTurnEngine`; there is no executable,
credential, environment, proxy or upstream configuration. User message content
is never used to generate the synthetic response. The default model is
`fake-model`; response IDs, text and zero usage are synthetic.

Importing `src/app/serve.ts` does not start anything. An explicit
`serveFakeRuntime()` call creates a fake listener on `127.0.0.1` and an ephemeral
port; the only other accepted hostname is literal `::1`. The returned `stop()`
cancels work and awaits listener shutdown, or rejects if shutdown is incomplete.
The default unit suite mocks
`Bun.serve`; only the explicitly invoked loopback smoke opens a socket.

### Known Bun 1.3.9 shutdown limitation

The listener starts native `server.stop(true)` before runtime cleanup. This
avoids a reproduced client-close race caused by issuing another graceful close
first. It still awaits both sides; it does not discard the native stop promise.

Separately, a server-initiated close (including the required post-START 1011)
can leave Bun 1.3.9 `pendingWebSockets` at 1 after both peers report closed,
preventing native stop from resolving. This reproduced with standalone Bun and
Node clients, not just a same-process fixture. A similar upstream report is
[Bun issue #36223](https://github.com/oven-sh/bun/issues/36223), reported for
1.3.14; that issue's closed status does not establish a fix for our pinned build.

`shutdownTimeoutMs` defaults to 3000 (allowed 1–60000). An unfinished native or
runtime stop rejects with `FakeShutdownTimeoutError`, code
`FAKE_SHUTDOWN_TIMEOUT`; this is a failure, never successful shutdown.
`stats().shutdown` becomes `failed`, not `stopped`. Engine `closed: true` or
zero active turns alone does not prove native listener termination. This guard
does not repair Bun's bookkeeping, release a stuck native handle or forcibly
exit the host process. The server-close shutdown acceptance gate remains open.
Wire close codes are preserved; there is no sleep, success timeout fallback or
unverified toolchain upgrade.

The upstream fix and a completed macOS arm64 Bun 1.4.2 candidate validation are
documented in [the isolated validation record](docs/bun-stop-fix-validation.md).
The candidate passed the complete demo suite and five independent runs of each
normal/server-1011 close path. The user authorized isolated validation only:
system/project/CI remain pinned to 1.3.9, whose control probe still fails. No
Linux or production validation, version replacement or deployment was performed.

Run the separate release gate with `bun run test/shutdown-regression.smoke.ts`
(or repository-root `make -C recovery runtime-shutdown-gate`). It uses synthetic
bytes on a temporary loopback port and exits nonzero on the pinned affected
runtime. It is intentionally not part of the normal-path `check-demo` result;
a green demo check does not clear this gate. Its watchdog only terminates its
own probe process, never the host application or a production service.

Local-demo safety policy, not claimed original behavior: both HTTP and WS
upgrade require the exact bound loopback Host/port. When Origin is present it
must exactly match that HTTP origin; remote/DNS-rebind hosts, cross-origin and
`null` origins are rejected. Origin-less local CLI requests remain allowed.
HTTP POST requires `application/json`; body reads have a cancellable 10-second
deadline. HTTP/WS idle timeouts are 15/30 seconds. This is not production auth.
The Bun fake listener does not support Vite or other reverse proxies; connect
directly to its returned loopback URL. Forwarded headers are not trusted.

Defaults: request/error/nonstream body cap 1 MiB, 4 active turns, 16 WS sessions,
8 KiB/128 pending beta tokens, 128 KiB Bun send buffer. Exhaustion rejects rather
than adding a work queue. Streaming produces one chunk per downstream pull or
drain; only nonstream/error output is collected. WS CANCEL emits no ACK/END and
reuse is permitted after `session.idle()` cleanup. Concurrent REQUEST emits
ERROR 409. Successful WS output is START/CHUNK(s)/END; a post-START stream failure
closes 1011. A concurrent write during backpressure closes 1013 as a deliberate
fake-only bounded-buffer policy.

The listener rejects large input before application allocation: Bun's native
HTTP body cap equals the configured request cap, and its WS message cap is
`min(64 MiB, max(request cap, 8 KiB) + 1 tag byte)`—1 MiB + 1 byte by default.
This intentionally smaller fake listener policy does not change the pure
codec's 64 MiB boundary or claim original listener parity. Bun-native rejections
(for example an early HTTP 413 or oversized WS close) may bypass the application
handler and therefore may not include its fake marker/error envelope.

## Evidence and compatibility limits

Static evidence is the privately recovered `isthmus.readable.mjs`:

- Lines 30262 and 30273–30282: known tags, byte-preserving encoder and decoder.
- Lines 30350–30394: client tag dispatch; CANCEL ignores payload; empty BETA
  families and USAGE_LIMIT are interpreted outside the raw decoder.
- Lines 30410–30513: server RESPONSE_START/CHUNK/END/KEEPALIVE/ERROR emission.
- Line 30544: Bun listener `maxPayloadLength: 67108864`.

The original raw helpers do not themselves enforce direction or size; tag
dispatch and the listener do. This foundation consolidates those checks into
the pure codec and applies the 64 MiB ceiling to both directions. That symmetric
outbound limit is an explicit local policy, not a claim that the original raw
encoder capped outbound chunks. Transport tests cover synthetic text/binary
frames, event order, cancellation and bounded backpressure. Live fragmentation,
compression and semantic compatibility still require separate evidence.

The fake slice intentionally does not parse real SSE into a non-streaming
Message; its driver supplies synthetic SSE or JSON directly. Still unimplemented:
real upstream turns, gRPC listener/generated bindings, CLI/PTY, production
pool/session lifecycle, MCP, OAuth/Vault, keepalive/compression parity, message
rewriting/MITM, deployment, migration and live compatibility. Passing these tests
does not make an execution slot healthy or authorize enabling the new production
execution plane.
