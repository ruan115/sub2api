import { describe, expect, test } from "bun:test";
import { collect, requestBytes } from "../../../test/fixtures";
import { FakeTurnEngine } from "./fake";
import { parseTurnRequest, usageLimit } from "./request";

describe("deterministic fake turn engine", () => {
  test("pulls one chunk at a time and keeps UTF-8 bytes intact across splits", async () => {
    const engine = new FakeTurnEngine({ responseText: "你好 🌍", chunkBytes: 1, delayMs: 0 });
    const turn = await engine.start(parseTurnRequest(requestBytes(false)), new AbortController().signal);
    expect(engine.stats().producedChunks).toBe(0);
    const first = await turn.chunks.next();
    expect(first.value).toEqual(new Uint8Array([123]));
    expect(engine.stats()).toMatchObject({ active: 1, completed: 0, producedChunks: 1 });
    await Promise.resolve();
    expect(engine.stats().producedChunks).toBe(1);
    const rest = await collect(turn.chunks);
    const body = new TextDecoder("utf-8", { fatal: true }).decode(new Uint8Array([123, ...rest]));
    expect(JSON.parse(body).content[0].text).toBe("你好 🌍");
    expect(body).not.toContain("SYNTHETIC_INPUT_DO_NOT_ECHO");
    expect(await turn.done).toBe("completed");
    expect(engine.stats().active).toBe(0);
    await turn.cancel();
    await engine.close();
    expect(engine.stats()).toMatchObject({ completed: 1, cancelled: 0, closed: true });
  });

  test("same inputs/options produce deterministic synthetic bytes", async () => {
    const outputs = [];
    for (let i = 0; i < 2; i++) {
      const engine = new FakeTurnEngine({ delayMs: 0, chunkBytes: 65536 });
      const turn = await engine.start(parseTurnRequest(requestBytes()), new AbortController().signal);
      outputs.push(await collect(turn.chunks));
      await engine.close();
    }
    expect(outputs[0]).toEqual(outputs[1]);
  });

  test("cancel before reading is idempotent and releases capacity", async () => {
    const engine = new FakeTurnEngine({ maxActiveTurns: 1, delayMs: 0 });
    const input = parseTurnRequest(requestBytes());
    const first = await engine.start(input, new AbortController().signal);
    await expect(engine.start(input, new AbortController().signal)).rejects.toMatchObject({ status: 429 });
    await first.cancel();
    await first.cancel();
    expect(await first.done).toBe("cancelled");
    expect(await first.chunks.next()).toMatchObject({ done: true });
    const reused = await engine.start(input, new AbortController().signal);
    await engine.close();
    expect(await reused.done).toBe("cancelled");
    expect(engine.stats()).toMatchObject({ active: 0, started: 2, cancelled: 2 });
    await expect(engine.start(input, new AbortController().signal)).rejects.toMatchObject({ status: 503 });
  });

  test("abort interrupts an outstanding delay and cleans up", async () => {
    const engine = new FakeTurnEngine({ delayMs: 1000 });
    const controller = new AbortController();
    const turn = await engine.start(parseTurnRequest(requestBytes()), controller.signal);
    const reading = turn.chunks.next();
    controller.abort();
    await expect(reading).rejects.toMatchObject({ status: 499 });
    expect(await turn.done).toBe("cancelled");
    expect(engine.stats()).toMatchObject({ active: 0, producedChunks: 0 });
    await engine.close();
  });

  test("pre-aborted requests do not reserve a turn", async () => {
    const engine = new FakeTurnEngine();
    const signal = AbortSignal.abort();
    await expect(engine.start(parseTurnRequest(requestBytes()), signal)).rejects.toMatchObject({ status: 499 });
    expect(engine.stats().started).toBe(0);
    await engine.close();
  });

  test("rejects unsafe fake bounds without reading external configuration", () => {
    for (const options of [{ chunkBytes: 0 }, { delayMs: 1001 }, { maxActiveTurns: 65 }, { responseText: "x".repeat(8193) }]) {
      expect(() => new FakeTurnEngine(options)).toThrow();
    }
    expect(usageLimit(" valid-test.1 ")).toBe("valid-test.1");
    expect(usageLimit("invalid/value")).toBeUndefined();
    expect(usageLimit("a".repeat(65))).toBeUndefined();
  });
});
