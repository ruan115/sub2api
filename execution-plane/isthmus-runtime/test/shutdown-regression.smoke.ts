// Explicit release gate, NOT the default offline suite or normal demo smoke.
// Bun 1.3.9 is known to fail this after server-initiated WS closure. A failure
// remains nonzero; do not change the expected wire code or swallow the timeout.
import assert from "node:assert/strict";
import { serveFakeRuntime } from "../src/app/serve";
import { CLIENT_FRAME_TAGS as CLIENT, SERVER_FRAME_TAGS as SERVER, decodeFrame, encodeFrame } from "../src/protocol/websocket";

const app = serveFakeRuntime({ scenario: "failure-after-first-chunk", delayMs: 0, chunkBytes: 32 });
const watchdog = setTimeout(() => {
  console.error("shutdown gate FAIL: owned probe process exceeded its lifecycle deadline");
  process.exit(2);
}, 10_000);
let exitCode = 0;
try {
  await new Promise<void>((resolve, reject) => {
    const socket = new WebSocket(app.url.replace("http:", "ws:") + "/v1/messages");
    socket.binaryType = "arraybuffer";
    const tags: number[] = [];
    const timer = setTimeout(() => { socket.close(); reject(new Error("Probe connection deadline")); }, 3000);
    socket.onopen = () => socket.send(encodeFrame("client-to-server", CLIENT.REQUEST,
      new TextEncoder().encode(JSON.stringify({ messages: [{ role: "user", content: "synthetic" }], stream: true }))));
    socket.onerror = () => { clearTimeout(timer); reject(new Error("Probe connection failed")); };
    socket.onmessage = (event) => {
      try { tags.push(decodeFrame("server-to-client", new Uint8Array(event.data as ArrayBuffer)).tag); }
      catch (error) { clearTimeout(timer); socket.close(); reject(error); }
    };
    socket.onclose = (event) => {
      clearTimeout(timer);
      try {
        assert.equal(event.code, 1011);
        assert.equal(tags[0], SERVER.RESPONSE_START);
        assert.ok(tags.includes(SERVER.CHUNK));
        assert.ok(!tags.includes(SERVER.END));
        resolve();
      } catch (error) { reject(error); }
    };
  });
  await app.stop();
  assert.equal(app.stats().failed, 1);
  assert.equal(app.stats().active, 0);
  assert.equal(app.stats().sessions, 0);
  console.log("shutdown gate PASS: server 1011 close followed by confirmed native stop");
} catch {
  exitCode = 1;
  console.error("shutdown gate FAIL: server-initiated close/stop could not be verified; see runtime README");
} finally {
  try { await app.stop(); } catch { exitCode = 1; }
  clearTimeout(watchdog);
}
// Only this dedicated probe exits. A native Bun shutdown bug may retain a
// handle even after stop rejects; never leave its temporary listener process.
process.exit(exitCode);
