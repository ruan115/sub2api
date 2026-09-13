import { describe, expect, test } from "bun:test";
import { CLIENT_FRAME_TAGS as CLIENT, SERVER_FRAME_TAGS as SERVER, encodeFrame } from "../../protocol/websocket";
import { FakeTurnEngine } from "../../runtime/turn/fake";
import type { TurnControls, TurnEngine } from "../../runtime/turn/types";
import { collect, joinBytes, requestBytes, requestFrame, TestPeer, utf8 } from "../../../test/fixtures";
import { parseTurnRequest } from "../../runtime/turn/request";
import { WebSocketSession } from "./session";

function envelope(peer: TestPeer, index = peer.frames.length - 1) {
  return JSON.parse(new TextDecoder().decode(peer.frames[index].payload));
}
const cancel = () => encodeFrame("client-to-server", CLIENT.CANCEL, utf8("ignored synthetic payload"));

describe("offline WebSocket fake session", () => {
  test("START/chunks/empty END preserve exact engine bytes including UTF-8 splits", async () => {
    const options = { responseText: "你好 🌍", chunkBytes: 3, delayMs: 0 };
    const engine = new FakeTurnEngine(options);
    const reference = new FakeTurnEngine(options);
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await peer.until(() => peer.frames.some((frame) => frame.tag === SERVER.CHUNK));
    expect(engine.stats()).toMatchObject({ active: 1, completed: 0 });
    expect(peer.frames.some((frame) => frame.tag === SERVER.END)).toBe(false);
    await session.idle();
    expect(peer.frames[0].tag).toBe(SERVER.RESPONSE_START);
    expect(envelope(peer, 0)).toEqual({ status: 200, statusText: "OK", headers: { "cache-control": "no-store", "content-type": "text/event-stream", "x-isthmus-runtime": "fake" } });
    expect(peer.frames.at(-1)).toEqual({ tag: SERVER.END, payload: new Uint8Array() });
    const expected = await reference.start(parseTurnRequest(requestBytes()), new AbortController().signal);
    const actualBytes = joinBytes(peer.frames.filter((frame) => frame.tag === SERVER.CHUNK).map((frame) => frame.payload));
    expect(actualBytes).toEqual(await collect(expected.chunks));
    expect(new TextDecoder("utf-8", { fatal: true }).decode(actualBytes)).toContain("你好 🌍");
    expect(session.busy).toBe(false);
    await session.close(); await engine.close(); await reference.close();
  });

  test("same-tick second request gets 409; cancellation has no ACK/END and permits reuse", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0, chunkBytes: 64 });
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    expect(session.busy).toBe(true);
    session.receive(requestFrame());
    expect(peer.frames[0].tag).toBe(SERVER.ERROR);
    expect(envelope(peer, 0)).toMatchObject({ status: 409, statusText: "Conflict" });
    expect(JSON.parse(envelope(peer, 0).body).error.type).toBe("invalid_request_error");
    session.receive(cancel());
    await session.idle();
    expect(peer.frames.some((frame) => frame.tag === SERVER.END)).toBe(false);
    expect(engine.stats()).toMatchObject({ started: 1, active: 0, cancelled: 1 });
    const oldCount = peer.frames.length;
    session.receive(requestFrame(false));
    await session.idle();
    expect(peer.frames[oldCount].tag).toBe(SERVER.RESPONSE_START);
    expect(peer.frames.at(-1)?.tag).toBe(SERVER.END);
    expect(engine.stats()).toMatchObject({ started: 2, completed: 1, active: 0 });
    await session.close(); await engine.close();
  });

  test("cancel after a chunk suppresses later output", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0, chunkBytes: 32 });
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await peer.until(() => peer.frames.some((frame) => frame.tag === SERVER.CHUNK));
    session.receive(cancel());
    const count = peer.frames.length;
    await session.idle();
    expect(peer.frames.length).toBe(count);
    expect(peer.frames.some((frame) => frame.tag === SERVER.END)).toBe(false);
    expect(engine.stats()).toMatchObject({ active: 0, cancelled: 1 });
    await session.close(); await engine.close();
  });

  test("cancel during backpressure releases a single drain wait and connection is reusable", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0 });
    const peer = new TestPeer(); peer.result = -1;
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await peer.until(() => peer.frames.length === 1);
    expect(session.waitingForDrain).toBe(true);
    expect(engine.stats().producedChunks).toBe(0);
    session.receive(cancel());
    await session.idle();
    expect(session.waitingForDrain).toBe(false);
    expect(session.isClosed).toBe(false);
    peer.result = 1;
    session.receive(requestFrame(false));
    await session.idle();
    expect(peer.frames.at(-1)?.tag).toBe(SERVER.END);
    await session.close(); await engine.close();
  });

  test("busy while backpressured closes on finite-buffer policy", async () => {
    const engine = new FakeTurnEngine();
    const peer = new TestPeer(); peer.result = -1;
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await peer.until(() => peer.frames.length === 1);
    session.receive(requestFrame());
    await session.idle();
    expect(session.isClosed).toBe(true);
    expect(peer.closes[0].code).toBe(1013);
    expect(peer.frames.length).toBe(1);
    expect(engine.stats().active).toBe(0);
    await session.close(); await engine.close();
  });

  test("control metadata is consumed once and busy clears pending controls", async () => {
    const underlying = new FakeTurnEngine({ delayMs: 0 });
    const captured: TurnControls[] = [];
    const engine: TurnEngine = { mode: "fake", start(request, signal) { captured.push(request.controls); return underlying.start(request, signal); }, close: () => underlying.close() };
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer);
    session.receive(encodeFrame("client-to-server", CLIENT.BETA_REPLACE));
    session.receive(encodeFrame("client-to-server", CLIENT.USAGE_LIMIT, utf8("synthetic-limit")));
    session.receive(requestFrame(false));
    session.receive(encodeFrame("client-to-server", CLIENT.BETA, utf8("discard-on-busy")));
    session.receive(requestFrame(false));
    await session.idle();
    session.receive(requestFrame(false));
    await session.idle();
    expect(captured).toEqual([{ replaceBeta: [], usageLimit: "synthetic-limit" }, {}]);
    await session.close(); await engine.close();
  });

  test("malformed/body-limit errors do not reserve turns; request payload is copied", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0 });
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer, { bodyLimitBytes: 256 });
    for (const frame of [new Uint8Array(), new Uint8Array([255]), new Uint8Array([SERVER.CHUNK]), encodeFrame("client-to-server", CLIENT.REQUEST, utf8("SYNTHETIC_PRIVATE_INVALID"))]) {
      session.receive(frame);
      await session.idle();
      expect(envelope(peer).status).toBe(400);
      expect(envelope(peer).body).not.toContain("SYNTHETIC_PRIVATE_INVALID");
    }
    session.receive(encodeFrame("client-to-server", CLIENT.REQUEST, new Uint8Array(257)));
    expect(envelope(peer).status).toBe(413);
    expect(engine.stats().started).toBe(0);
    const frame = requestFrame(false);
    session.receive(frame);
    frame.fill(255);
    await session.idle();
    expect(peer.frames.at(-1)?.tag).toBe(SERVER.END);
    await session.close(); await engine.close();
  });

  test("non-2xx ERROR includes string body without START/END", async () => {
    const engine = new FakeTurnEngine({ scenario: "upstream-error", delayMs: 0 });
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await session.idle();
    expect(peer.frames.length).toBe(1);
    expect(peer.frames[0].tag).toBe(SERVER.ERROR);
    expect(envelope(peer)).toMatchObject({ status: 503, statusText: "Service Unavailable", headers: { "content-type": "application/json", "x-isthmus-runtime": "fake" } });
    expect(JSON.parse(envelope(peer).body).error.type).toBe("overloaded_error");
    expect(engine.stats().active).toBe(0);
    await session.close(); await engine.close();
  });

  test("post-START failure closes 1011 without misleading END/ERROR", async () => {
    const engine = new FakeTurnEngine({ scenario: "failure-after-first-chunk", chunkBytes: 16, delayMs: 0 });
    const peer = new TestPeer();
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await session.idle();
    expect(peer.frames.map((frame) => frame.tag)).toEqual([SERVER.RESPONSE_START, SERVER.CHUNK]);
    expect(peer.closes).toEqual([{ code: 1011, reason: "fake stream failed" }]);
    expect(session.isClosed).toBe(true);
    expect(engine.stats()).toMatchObject({ active: 0, failed: 1 });
    await session.close(); await engine.close();
  });

  test("connection close aborts work, clears drain and ignores later frames", async () => {
    const engine = new FakeTurnEngine({ delayMs: 1000 });
    const peer = new TestPeer(); peer.result = -1;
    const session = new WebSocketSession(engine, peer);
    session.receive(requestFrame());
    await peer.until(() => peer.frames.length === 1);
    await session.close();
    await session.close();
    expect(session.waitingForDrain).toBe(false);
    expect(engine.stats()).toMatchObject({ active: 0, cancelled: 1 });
    session.receive(requestFrame());
    expect(peer.frames.length).toBe(1);
    expect(peer.closes.length).toBe(1);
    await engine.close();
  });
});
