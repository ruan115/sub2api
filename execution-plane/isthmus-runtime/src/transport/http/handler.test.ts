import { describe, expect, test } from "bun:test";
import { FakeTurnEngine } from "../../runtime/turn/fake";
import type { TurnEngine } from "../../runtime/turn/types";
import { requestBytes, utf8 } from "../../../test/fixtures";
import { createHttpHandler } from "./handler";

function post(body: BodyInit = requestBytes(), signal?: AbortSignal, headers?: HeadersInit): Request {
  const inputHeaders = new Headers(headers);
  if (!inputHeaders.has("content-type")) inputHeaders.set("content-type", "application/json");
  return new Request("http://fake.invalid/v1/messages", { method: "POST", body, signal, headers: inputHeaders });
}

describe("offline HTTP fake transport", () => {
  test("response/first chunk precede completion and production follows demand", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0, chunkBytes: 32 });
    const response = await createHttpHandler(engine)(post());
    expect(response.headers.get("x-isthmus-runtime")).toBe("fake");
    expect(response.headers.get("content-type")).toBe("text/event-stream");
    expect(engine.stats().producedChunks).toBe(0);
    const reader = response.body!.getReader();
    const first = await reader.read();
    expect(first.done).toBe(false);
    expect(engine.stats()).toMatchObject({ active: 1, completed: 0, producedChunks: 1 });
    await Promise.resolve();
    expect(engine.stats().producedChunks).toBe(1);
    await reader.cancel();
    reader.releaseLock();
    expect(engine.stats()).toMatchObject({ active: 0, cancelled: 1 });
    await engine.close();
  });

  test("nonstreaming JSON and complete streaming SSE retain synthetic UTF-8", async () => {
    const engine = new FakeTurnEngine({ responseText: "你好 🌍", delayMs: 0, chunkBytes: 3 });
    const handler = createHttpHandler(engine);
    const json = await handler(post(requestBytes(false)));
    expect(json.status).toBe(200);
    expect((await json.json()).content[0].text).toBe("你好 🌍");
    const stream = await handler(post());
    const text = await stream.text();
    expect(text).toContain('"text":"你好 🌍"');
    expect(text).toContain("event: message_stop\n");
    expect(text).not.toContain("SYNTHETIC_INPUT_DO_NOT_ECHO");
    expect(engine.stats()).toMatchObject({ active: 0, completed: 2 });
    await engine.close();
  });

  test("abort works before reading and during an outstanding pull", async () => {
    const engine = new FakeTurnEngine({ delayMs: 1000 });
    const handler = createHttpHandler(engine);
    for (const shouldRead of [false, true]) {
      const controller = new AbortController();
      const response = await handler(post(requestBytes(), controller.signal));
      const reader = response.body!.getReader();
      const pending = shouldRead ? reader.read() : undefined;
      controller.abort();
      await expect(pending ?? reader.read()).rejects.toMatchObject({ status: 499 });
      reader.releaseLock();
    }
    await engine.close();
    expect(engine.stats()).toMatchObject({ active: 0, cancelled: 2, producedChunks: 0 });
  });

  test("pre-abort, malformed bytes and invalid shapes have bounded public errors", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0 });
    const handler = createHttpHandler(engine);
    for (const body of [utf8("not-json-SYNTHETIC_PRIVATE"), new Uint8Array([0xff]), utf8("null"), utf8("[]"), utf8('{"messages":[]}'), utf8(JSON.stringify({ messages: [1], model: "a".repeat(129) }))]) {
      const response = await handler(post(body));
      expect(response.status).toBe(400);
      expect(await response.text()).not.toContain("SYNTHETIC_PRIVATE");
    }
    expect((await handler(post(requestBytes(), AbortSignal.abort()))).status).toBe(499);
    expect(engine.stats().started).toBe(0);
    await engine.close();
  });

  test("counts chunked request bytes before retention and cancels over-limit source", async () => {
    const engine = new FakeTurnEngine();
    let cancelled = 0;
    let produced = 0;
    const source = new ReadableStream<Uint8Array>({
      pull(controller) { produced++; controller.enqueue(utf8("12345678")); },
      cancel() { cancelled++; },
    }, { highWaterMark: 0 });
    const response = await createHttpHandler(engine, { bodyLimitBytes: 12 })(post(source));
    expect(response.status).toBe(413);
    expect(produced).toBe(2);
    expect(cancelled).toBe(1);
    expect(engine.stats().started).toBe(0);
    const declared = await createHttpHandler(engine, { bodyLimitBytes: 12 })(post("{}", undefined, { "content-length": "999" }));
    expect(declared.status).toBe(413);
    await engine.close();
  });

  test("aborting a pending request-body read cancels its source", async () => {
    const engine = new FakeTurnEngine();
    let started!: () => void;
    const pulling = new Promise<void>((resolve) => { started = resolve; });
    let cancelled = 0;
    const source = new ReadableStream<Uint8Array>({ pull() { started(); }, cancel() { cancelled++; } }, { highWaterMark: 0 });
    const controller = new AbortController();
    const pending = createHttpHandler(engine)(post(source, controller.signal));
    await pulling;
    controller.abort();
    expect((await pending).status).toBe(499);
    expect(cancelled).toBe(1);
    expect(engine.stats().started).toBe(0);
    await engine.close();
  });

  test("body read deadline cancels stalled input with 408 before reserving a turn", async () => {
    const engine = new FakeTurnEngine();
    let cancelled = false;
    const source = new ReadableStream<Uint8Array>({ pull() {}, cancel() { cancelled = true; } }, { highWaterMark: 0 });
    const response = await createHttpHandler(engine, { bodyReadTimeoutMs: 5 })(post(source));
    expect(response.status).toBe(408);
    expect(cancelled).toBe(true);
    expect(engine.stats().started).toBe(0);
    await engine.close();
  });

  test("local-demo policy requires application/json, blocking simple form/no-cors posts", async () => {
    const engine = new FakeTurnEngine({ delayMs: 0 });
    const handler = createHttpHandler(engine);
    for (const type of ["text/plain", "application/x-www-form-urlencoded", "multipart/form-data"]) {
      expect((await handler(post(requestBytes(false), undefined, { "content-type": type }))).status).toBe(415);
    }
    const missing = new Request("http://fake.invalid/v1/messages", { method: "POST", body: requestBytes(false) });
    expect((await handler(missing)).status).toBe(415);
    expect(engine.stats().started).toBe(0);
    const valid = await handler(post(requestBytes(false), undefined, { "content-type": "application/json; charset=utf-8" }));
    expect(valid.status).toBe(200);
    await valid.text();
    await engine.close();
  });

  test("upstream error and collection limit are bounded and clean up", async () => {
    const unavailable = new FakeTurnEngine({ scenario: "upstream-error", delayMs: 0 });
    const response = await createHttpHandler(unavailable)(post());
    expect(response.status).toBe(503);
    expect(response.headers.get("content-type")).toBe("application/json");
    expect((await response.json()).error.type).toBe("overloaded_error");
    await unavailable.close();
    const limited = new FakeTurnEngine({ delayMs: 0, chunkBytes: 64 });
    expect((await createHttpHandler(limited, { responseLimitBytes: 20 })(post(requestBytes(false)))).status).toBe(502);
    expect(limited.stats().active).toBe(0);
    await limited.close();
  });

  test("failure after first chunk errors the stream and releases resources", async () => {
    const engine = new FakeTurnEngine({ scenario: "failure-after-first-chunk", delayMs: 0, chunkBytes: 16 });
    const reader = (await createHttpHandler(engine)(post())).body!.getReader();
    expect((await reader.read()).done).toBe(false);
    await expect(reader.read()).rejects.toMatchObject({ status: 502 });
    reader.releaseLock();
    expect(engine.stats()).toMatchObject({ active: 0, failed: 1 });
    await engine.close();
  });

  test("health/routes are explicitly fake and unexpected exceptions are sanitized", async () => {
    const engine: TurnEngine = { mode: "fake", async start() { throw new Error("SYNTHETIC_PRIVATE_EXCEPTION"); }, async close() {} };
    const handler = createHttpHandler(engine);
    for (const route of ["/health", "/"]) {
      const response = await handler(new Request(`http://fake.invalid${route}`));
      expect(await response.json()).toEqual({ ok: true, mode: "fake", service: "isthmus-fake" });
    }
    expect((await handler(new Request("http://fake.invalid/missing"))).status).toBe(404);
    expect((await handler(new Request("http://fake.invalid/v1/messages"))).status).toBe(404);
    const response = await handler(post());
    expect(response.status).toBe(502);
    expect(await response.text()).not.toContain("SYNTHETIC_PRIVATE_EXCEPTION");
  });
});
