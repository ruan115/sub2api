import { describe, expect, spyOn, test } from "bun:test";
import { requestBytes, requestFrame, TestPeer } from "../../test/fixtures";
import { createFakeRuntime } from "./fake";

describe("pure fake application assembly", () => {
  test("import/assembly/turn have no network, process, file or credential side effects", async () => {
    const spies = [
      spyOn(globalThis, "fetch"), spyOn(Bun, "serve"), spyOn(Bun, "connect"),
      spyOn(Bun, "listen"), spyOn(Bun, "spawn"), spyOn(Bun, "spawnSync"), spyOn(Bun, "file"),
    ];
    for (const spy of spies) spy.mockImplementation((() => { throw new Error("Forbidden fake runtime capability"); }) as never);
    try {
      const module = await import("./serve");
      expect(typeof module.serveFakeRuntime).toBe("function");
      const options = { delayMs: 0, chunkBytes: 65536, get credentials() { throw new Error("Credentials must never be read"); } };
      const runtime = createFakeRuntime(options);
      const response = await runtime.handleHttp(new Request("http://fake.invalid/v1/messages", {
        method: "POST", body: requestBytes(false),
        headers: { "content-type": "application/json", authorization: "SYNTHETIC_UNUSED_AUTH", "x-api-key": "SYNTHETIC_UNUSED_KEY" },
      }));
      const output = await response.text();
      expect(output).not.toContain("SYNTHETIC_UNUSED");
      expect(response.headers.get("x-isthmus-runtime")).toBe("fake");
      await runtime.close();
      expect(runtime.stats()).toMatchObject({ closed: true, active: 0, sessions: 0 });
      for (const spy of spies) expect(spy).not.toHaveBeenCalled();
    } finally { for (const spy of spies) spy.mockRestore(); }
  });

  test("session capacity rejects and closed connections free their slots", async () => {
    const runtime = createFakeRuntime({ maxSessions: 1 });
    const first = runtime.createWebSocketSession(new TestPeer());
    expect(() => runtime.createWebSocketSession(new TestPeer())).toThrow("Fake session capacity exhausted");
    await first.close();
    const second = runtime.createWebSocketSession(new TestPeer());
    expect(runtime.stats().sessions).toBe(1);
    await runtime.close();
    expect(second.isClosed).toBe(true);
    expect(runtime.stats().sessions).toBe(0);
    expect(() => runtime.createWebSocketSession(new TestPeer())).toThrow("Fake runtime is closed");
    expect((await runtime.handleHttp(new Request("http://fake.invalid/health"))).status).toBe(503);
  });

  test("shutdown cancels unread HTTP response, WS drain and is idempotent", async () => {
    const runtime = createFakeRuntime({ delayMs: 1000 });
    const http = await runtime.handleHttp(new Request("http://fake.invalid/v1/messages", { method: "POST", body: requestBytes(), headers: { "content-type": "application/json" } }));
    const peer = new TestPeer(); peer.result = -1;
    const session = runtime.createWebSocketSession(peer);
    session.receive(requestFrame());
    await peer.until(() => peer.frames.length === 1);
    expect(runtime.stats().active).toBe(2);
    const first = runtime.close();
    expect(runtime.close()).toBe(first);
    await first;
    await expect(http.text()).rejects.toMatchObject({ status: 499 });
    expect(session.waitingForDrain).toBe(false);
    expect(runtime.stats()).toMatchObject({ closed: true, active: 0, sessions: 0, cancelled: 2 });
  });

  test("shutdown cancels a pending request-body read before engine startup", async () => {
    const runtime = createFakeRuntime();
    let pulling!: () => void;
    const started = new Promise<void>((resolve) => { pulling = resolve; });
    let cancelled = false;
    const stream = new ReadableStream<Uint8Array>({ pull() { pulling(); }, cancel() { cancelled = true; } }, { highWaterMark: 0 });
    const response = runtime.handleHttp(new Request("http://fake.invalid/v1/messages", { method: "POST", body: stream, headers: { "content-type": "application/json" } }));
    await started;
    await runtime.close();
    expect((await response).status).toBe(499);
    expect(cancelled).toBe(true);
    expect(runtime.stats().started).toBe(0);
  });
});
