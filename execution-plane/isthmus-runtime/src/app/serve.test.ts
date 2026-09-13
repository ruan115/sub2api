import { describe, expect, spyOn, test } from "bun:test";
import { requestBytes, TestPeer } from "../../test/fixtures";
import { CLIENT_FRAME_TAGS, SERVER_FRAME_TAGS } from "../protocol/websocket";
import { DEFAULT_BODY_LIMIT_BYTES } from "../transport/shared/body";
import { MAX_CONTROL_BYTES } from "../transport/websocket/controls";
import { serveFakeRuntime } from "./serve";

describe("explicit loopback-only listener factory, without opening sockets", () => {
  test("rejects all nonliteral-loopback hosts and invalid ports before Bun.serve", () => {
    const listener = spyOn(Bun, "serve").mockImplementation((() => { throw new Error("must not listen"); }) as never);
    try {
      for (const hostname of ["0.0.0.0", "::", "localhost", "127.0.0.2", "fake.invalid"]) {
        expect(() => serveFakeRuntime({ hostname: hostname as never })).toThrow("literal loopback");
      }
      for (const port of [-1, 65536, 1.5, Number.NaN]) expect(() => serveFakeRuntime({ port })).toThrow("port");
      expect(listener).not.toHaveBeenCalled();
    } finally { listener.mockRestore(); }
  });

  test("default is loopback/ephemeral with bounded WS buffers; adapters and stop work", async () => {
    let config: any;
    let stops = 0;
    const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => {
      config = options;
      return { port: 43210, stop(force: boolean) { expect(force).toBe(true); stops++; } };
    }) as never);
    try {
      const server = serveFakeRuntime({ delayMs: 0 });
      expect(server.mode).toBe("fake");
      expect(server.url).toBe("http://127.0.0.1:43210");
      expect(config).toMatchObject({ hostname: "127.0.0.1", port: 0, idleTimeout: 15, maxRequestBodySize: DEFAULT_BODY_LIMIT_BYTES, websocket: { idleTimeout: 30, maxPayloadLength: DEFAULT_BODY_LIMIT_BYTES + 1, backpressureLimit: 131072, closeOnBackpressureLimit: true, perMessageDeflate: false } });
      const health = await config.fetch(new Request("http://127.0.0.1:43210/health"), { port: 43210 });
      expect((await health.json()).mode).toBe("fake");
      const upgrade = new Request("http://127.0.0.1:43210/v1/messages", { headers: { upgrade: "websocket" } });
      expect(await config.fetch(upgrade, { port: 43210, upgrade: () => true })).toBeUndefined();
      expect((await config.fetch(upgrade, { port: 43210, upgrade: () => false })).status).toBe(400);
      const peer = new TestPeer();
      const socket = { data: {} as any, readyState: WebSocket.OPEN, sendBinary: (frame: Uint8Array) => peer.sendBinary(frame), close: (code: number, reason: string) => peer.close(code, reason) };
      config.websocket.open(socket);
      const textFrame = String.fromCharCode(CLIENT_FRAME_TAGS.REQUEST) + new TextDecoder().decode(requestBytes(false));
      config.websocket.message(socket, textFrame);
      await socket.data.session.idle();
      expect(peer.frames[0].tag).toBe(SERVER_FRAME_TAGS.RESPONSE_START);
      expect(peer.frames.at(-1)?.tag).toBe(SERVER_FRAME_TAGS.END);
      config.websocket.drain(socket);
      config.websocket.close(socket);
      expect(server.stats().sessions).toBe(0);
      const stopping = server.stop();
      expect(server.stop()).toBe(stopping);
      await stopping;
      expect(stops).toBe(1);
      expect(server.stats()).toMatchObject({ closed: true, active: 0, sessions: 0 });
    } finally { listener.mockRestore(); }
  });

  test("explicit IPv6 loopback formats URL safely", async () => {
    let config: any;
    const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => { config = options; return { port: 23456, stop() {} }; }) as never);
    try {
      const server = serveFakeRuntime({ hostname: "::1" });
      expect(config.hostname).toBe("::1");
      expect(server.url).toBe("http://[::1]:23456");
      await server.stop();
    } finally { listener.mockRestore(); }
  });

  test("Host/Origin gate runs before both HTTP dispatch and WebSocket upgrade", async () => {
    let config: any;
    const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => { config = options; return { port: 43210, stop() {} }; }) as never);
    try {
      const server = serveFakeRuntime();
      let upgrades = 0;
      const transport = { port: 43210, upgrade() { upgrades++; return true; } };
      for (const headers of [{ origin: "http://evil.invalid" }, { origin: "null" }, { host: "rebind.invalid:43210" }]) {
        const plain = await config.fetch(new Request("http://127.0.0.1:43210/v1/messages", { method: "POST", headers: { ...headers, "content-type": "application/json" }, body: requestBytes(false) }), transport);
        expect(plain.status).toBe(403);
        const ws = await config.fetch(new Request("http://127.0.0.1:43210/v1/messages", { headers: { ...headers, upgrade: "websocket" } }), transport);
        expect(ws.status).toBe(403);
      }
      expect(upgrades).toBe(0);
      expect(server.stats().started).toBe(0);
      expect(server.stats().sessions).toBe(0);
      await server.stop();
    } finally { listener.mockRestore(); }
  });

  test("native HTTP/WS ingress limits follow configured fake budgets", async () => {
    let config: any;
    const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => { config = options; return { port: 43210, stop() {} }; }) as never);
    try {
      for (const bodyLimitBytes of [4, 16384, 16 * 1024 * 1024]) {
        const server = serveFakeRuntime({ bodyLimitBytes });
        expect(config.maxRequestBodySize).toBe(bodyLimitBytes);
        expect(config.websocket.maxPayloadLength).toBe(Math.max(bodyLimitBytes, MAX_CONTROL_BYTES) + 1);
        await server.stop();
      }
    } finally { listener.mockRestore(); }
  });

  test("stop awaits asynchronous listener shutdown and propagates its failure", async () => {
    for (const shouldFail of [false, true]) {
      let release!: () => void;
      let stopCalled!: () => void;
      const entered = new Promise<void>((resolve) => { stopCalled = resolve; });
      const held = new Promise<void>((resolve) => { release = resolve; });
      let stops = 0;
      const listener = spyOn(Bun, "serve").mockImplementation((() => ({
        port: 43210,
        async stop(force: boolean) {
          expect(force).toBe(true);
          stops++;
          stopCalled();
          await held;
          if (shouldFail) throw new Error("Synthetic listener shutdown failure");
        },
      })) as never);
      try {
        const server = serveFakeRuntime();
        let settled = false;
        const stopping = server.stop();
        const result = stopping.then(() => { settled = true; return "ok"; }, () => { settled = true; return "failed"; });
        await entered;
        expect(settled).toBe(false);
        expect(server.stop()).toBe(stopping);
        expect(server.stats()).toMatchObject({ closed: true, active: 0 });
        release();
        expect(await result).toBe(shouldFail ? "failed" : "ok");
        expect(stops).toBe(1);
      } finally { release(); listener.mockRestore(); }
    }
  });

  test("listener shutdown still runs after a synthetic turn failure", async () => {
    let config: any;
    let stops = 0;
    const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => { config = options; return { port: 43210, async stop() { stops++; } }; }) as never);
    try {
      const server = serveFakeRuntime({ scenario: "failure-after-first-chunk", delayMs: 0, chunkBytes: 16 });
      const peer = new TestPeer();
      const socket = { data: {} as any, readyState: WebSocket.OPEN, sendBinary: (frame: Uint8Array) => peer.sendBinary(frame), close: (code: number, reason: string) => peer.close(code, reason) };
      config.websocket.open(socket);
      config.websocket.message(socket, String.fromCharCode(CLIENT_FRAME_TAGS.REQUEST) + new TextDecoder().decode(requestBytes()));
      await socket.data.session.idle();
      expect(server.stats().failed).toBe(1);
      expect(peer.closes[0].code).toBe(1011);
      await server.stop();
      expect(stops).toBe(1);
      expect(server.stats()).toMatchObject({ closed: true, active: 0, sessions: 0 });
    } finally { listener.mockRestore(); }
  });

  test("native never-settles is failed shutdown, not a successful stopped state", async () => {
    const listener = spyOn(Bun, "serve").mockImplementation((() => ({ port: 43210, stop: () => new Promise<void>(() => {}) })) as never);
    try {
      const server = serveFakeRuntime({ shutdownTimeoutMs: 5 });
      const stopping = server.stop();
      expect(server.stats().shutdown).toBe("stopping");
      await expect(stopping).rejects.toMatchObject({ code: "FAKE_SHUTDOWN_TIMEOUT" });
      expect(server.stats()).toMatchObject({ closed: true, active: 0, sessions: 0, shutdown: "failed" });
      expect(server.stop()).toBe(stopping);
    } finally { listener.mockRestore(); }
  });

  test("already-closed native socket is not gracefully closed again by its callback", async () => {
    let config: any;
    let closeCalls = 0;
    const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => { config = options; return { port: 43210, stop() {} }; }) as never);
    try {
      const server = serveFakeRuntime();
      const socket = { data: {} as any, readyState: WebSocket.CLOSED, sendBinary: () => 1, close() { closeCalls++; } };
      config.websocket.open(socket);
      config.websocket.close(socket);
      await server.stop();
      expect(closeCalls).toBe(0);
      expect(server.stats().shutdown).toBe("stopped");
    } finally { listener.mockRestore(); }
  });
});
