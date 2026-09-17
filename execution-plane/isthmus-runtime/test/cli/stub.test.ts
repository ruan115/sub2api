import { describe, expect, test } from "bun:test";
import { createCliStub, STUB_BODY_LIMIT, SYNTHETIC_TEXT, SYNTHETIC_TOKEN } from "./stub";

function request(stub: ReturnType<typeof createCliStub>, body: unknown = {}, init: RequestInit = {}) {
  return new Request(`http://127.0.0.1:18000${stub.pathPrefix}/v1/messages?beta=true`, {
    method: "POST", headers: { authorization: `Bearer ${SYNTHETIC_TOKEN}`, "content-type": "application/json" },
    body: JSON.stringify({ model: "claude-sonnet-5", max_tokens: 128, stream: true, ...body as object }), ...init,
  });
}

describe("pure synthetic CLI upstream handler (no listening)", () => {
  test("six exact SSE frames and whitelisted summaries only", async () => {
    const stub = createCliStub({ frameDelayMs: 0 });
    try {
      const response = await stub.handle(request(stub, { messages: [{ role: "user", content: "synthetic private marker" }] }));
      expect(response.status).toBe(200);
      expect(response.headers.get("content-type")).toBe("text/event-stream");
      const frames = (await response.text()).trim().split("\n\n");
      expect(frames.map((frame) => frame.split("\n")[0])).toEqual([
        "event: message_start", "event: content_block_start", "event: content_block_delta",
        "event: content_block_stop", "event: message_delta", "event: message_stop",
      ]);
      expect(frames[2]).toContain(SYNTHETIC_TEXT);
      expect(stub.stats().active).toBe(0);
      expect(stub.stats().completed).toBe(1);
      expect(JSON.stringify(stub.stats())).not.toContain("synthetic private marker");
      expect(JSON.stringify(stub.stats())).not.toContain(SYNTHETIC_TOKEN);
    } finally { await stub.stop(); }
  });

  test("unary body and fixed synthetic usage", async () => {
    const stub = createCliStub();
    try {
      const value = await (await stub.handle(request(stub, { stream: false }))).json();
      expect(value.content).toEqual([{ type: "text", text: SYNTHETIC_TEXT }]);
      expect(value.usage).toEqual({ input_tokens: 1, output_tokens: 1, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 });
    } finally { await stub.stop(); }
  });

  test("rejects unscoped paths, real-shaped auth, invalid tokens and oversized bodies", async () => {
    const stub = createCliStub();
    try {
      expect((await stub.handle(new Request("http://127.0.0.1:18000/v1/messages"))).status).toBe(403);
      expect((await stub.handle(request(stub, {}, { headers: { authorization: "Bearer synthetic-other" } }))).status).toBe(403);
      for (const max_tokens of [0, 129, 1.5, "128", true]) expect((await stub.handle(request(stub, { max_tokens }))).status).toBe(400);
      const response = await stub.handle(request(stub, { padding: "x".repeat(STUB_BODY_LIMIT) }));
      expect(response.status).toBe(413);
      expect(stub.stats().requests).toBe(0);
    } finally { await stub.stop(); }
  });

  test("fixed ten-request budget and 503 do not create a successful stream", async () => {
    const stub = createCliStub({ failure503: true });
    try {
      for (let i = 0; i < 10; i++) expect((await stub.handle(request(stub))).status).toBe(503);
      expect((await stub.handle(request(stub))).status).toBe(429);
      expect(stub.stats()).toMatchObject({ requests: 10, failed: 10, active: 0, completed: 0 });
    } finally { await stub.stop(); }
  });

  test("cancel and stop release delayed stream without a completion", async () => {
    for (const stop of [false, true]) {
      const stub = createCliStub({ frameDelayMs: 30_000 });
      try {
        const response = await stub.handle(request(stub));
        await stub.waitForRequest();
        expect(stub.stats().active).toBe(1);
        if (stop) await stub.stop(); else await response.body!.cancel();
        expect(stub.stats()).toMatchObject({ active: 0, completed: 0, cancelled: 1 });
      } finally { await stub.stop(); }
    }
  });
});
