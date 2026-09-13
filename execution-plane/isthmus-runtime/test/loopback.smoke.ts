// Explicit optional smoke test: only newly written fake code, an ephemeral
// loopback listener and synthetic bytes. Not part of the offline `bun test` run.
import assert from "node:assert/strict";
import { serveFakeRuntime } from "../src/app/serve";
import { CLIENT_FRAME_TAGS as CLIENT, SERVER_FRAME_TAGS as SERVER, decodeFrame, encodeFrame } from "../src/protocol/websocket";

const app = serveFakeRuntime({ chunkBytes: 32, delayMs: 5 });
// Bounds the whole owned smoke process, including a regressed native shutdown.
// Process exit also releases this test's ephemeral port; no external process is touched.
const watchdog = setTimeout(() => {
  console.error("loopback smoke FAIL: lifecycle deadline exceeded");
  process.exit(2);
}, 10_000);
const payload = new TextEncoder().encode(JSON.stringify({
  model: "fake-model", stream: true,
  messages: [{ role: "user", content: "synthetic smoke only" }],
}));
const request = (path: string, options: RequestInit = {}) =>
  fetch(app.url + path, { ...options, signal: AbortSignal.timeout(3000) });

try {
  assert.equal((await (await request("/health")).json()).mode, "fake");
  for (const headers of [{ origin: "http://evil.invalid" }, { host: "rebind.invalid" }]) {
    const rejected = await request("/health", { headers });
    assert.equal(rejected.status, 403);
    await rejected.arrayBuffer();
  }
  const mediaType = await request("/v1/messages", {
    method: "POST", headers: { "content-type": "text/plain" }, body: "{}",
  });
  assert.equal(mediaType.status, 415);
  await mediaType.arrayBuffer();
  const response = await request("/v1/messages", {
    method: "POST", headers: { "content-type": "application/json" }, body: payload,
  });
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("x-isthmus-runtime"), "fake");
  const reader = response.body!.getReader();
  assert.equal((await reader.read()).done, false);
  assert.equal(app.stats().completed, 0, "first chunk must precede completion");
  await reader.cancel();

  await new Promise<void>((resolve, reject) => {
    const socket = new WebSocket(app.url.replace("http:", "ws:") + "/v1/messages");
    socket.binaryType = "arraybuffer";
    const tags: number[] = [];
    const fail = (error: unknown) => { clearTimeout(timer); socket.close(); reject(error); };
    const timer = setTimeout(() => fail(new Error("Fake WS deadline exceeded")), 3000);
    socket.onopen = () => socket.send(encodeFrame("client-to-server", CLIENT.REQUEST, payload));
    socket.onerror = () => fail(new Error("Fake WS connection failed"));
    socket.onmessage = (event) => {
      try {
        const frame = decodeFrame("server-to-client", new Uint8Array(event.data as ArrayBuffer));
        tags.push(frame.tag);
        if (frame.tag === SERVER.END) {
          assert.equal(tags[0], SERVER.RESPONSE_START);
          assert.ok(tags.includes(SERVER.CHUNK));
          socket.close();
        }
      } catch (error) { fail(error); }
    };
    socket.onclose = () => {
      clearTimeout(timer);
      if (tags.at(-1) === SERVER.END) resolve();
      else reject(new Error("Fake WS stream truncated"));
    };
  });
  console.log("loopback transport checks PASS; awaiting listener shutdown");
} finally {
  try {
    await app.stop();
    assert.equal(app.stats().active, 0);
    assert.equal(app.stats().sessions, 0);
    console.log("loopback shutdown PASS: no active turns or sessions");
  } finally { clearTimeout(watchdog); }
}
