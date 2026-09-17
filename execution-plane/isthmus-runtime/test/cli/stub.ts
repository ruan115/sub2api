// Synthetic-only upstream. Importing this module never opens a listener.
import { stopWithDeadline } from "../../src/app/shutdown";

export const SYNTHETIC_TOKEN = "synthetic-probe-token-not-a-real-credential";
export const SYNTHETIC_TEXT = "CLI_ROUNDTRIP_OK";
export const STUB_BODY_LIMIT = 256 * 1024;
const MAX_REQUESTS = 10;
const encoder = new TextEncoder();
type Outcome = "completed" | "cancelled" | "failed";
export interface StubOptions { readonly frameDelayMs?: number; readonly failure503?: boolean }

function events() {
  const usage = { input_tokens: 1, output_tokens: 0, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 };
  return [
    { type: "message_start", message: { id: "msg_cli_synthetic", type: "message", role: "assistant",
      model: "claude-sonnet-5", content: [], stop_reason: null, stop_sequence: null, usage } },
    { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
    { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: SYNTHETIC_TEXT } },
    { type: "content_block_stop", index: 0 },
    { type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 1 } },
    { type: "message_stop" },
  ].map((event) => encoder.encode(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`));
}

function delay(milliseconds: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) { reject(new Error("synthetic_stub_cancelled")); return; }
    const aborted = () => { clearTimeout(timer); signal.removeEventListener("abort", aborted); reject(new Error("synthetic_stub_cancelled")); };
    const timer = setTimeout(() => { signal.removeEventListener("abort", aborted); resolve(); }, milliseconds);
    signal.addEventListener("abort", aborted, { once: true });
  });
}

/** Pure handler for default unit tests. No network client and no retained bodies. */
export function createCliStub(options: StubOptions = {}) {
  const frameDelay = options.frameDelayMs ?? 5;
  if (!Number.isInteger(frameDelay) || frameDelay < 0 || frameDelay > 30_000) throw new Error("invalid_stub_delay");
  const pathPrefix = `/capture-${crypto.randomUUID().replaceAll("-", "")}`;
  const counts = { requests: 0, rejected: 0, active: 0, completed: 0, cancelled: 0, failed: 0 };
  let last: { stream: boolean; maxTokens: number; bodyBytes: number; synthetic: true } | undefined;
  let stopped = false;
  const running = new Set<AbortController>();
  const waiters = new Set<() => void>();
  const reject = (status: number, reason: string) => {
    counts.rejected++;
    return Response.json({ type: "error", error: { type: "invalid_request_error", message: reason } }, { status });
  };
  return {
    pathPrefix,
    stats: () => ({ ...counts, last: last && { ...last }, stopped }),
    waitForRequest(timeoutMs = 10_000): Promise<void> {
      if (counts.requests > 0) return Promise.resolve();
      if (stopped) return Promise.reject(new Error("synthetic_stub_stopped"));
      return new Promise((resolve, rejectWait) => {
        const done = () => {
          clearTimeout(timer); waiters.delete(done);
          if (stopped && counts.requests === 0) rejectWait(new Error("synthetic_stub_stopped"));
          else resolve();
        };
        const timer = setTimeout(() => { waiters.delete(done); rejectWait(new Error("synthetic_request_deadline")); }, timeoutMs);
        waiters.add(done);
      });
    },
    async handle(request: Request): Promise<Response> {
      const url = new URL(request.url);
      if (stopped) return reject(503, "synthetic_stub_stopped");
      if (url.hostname !== "127.0.0.1" || request.headers.has("origin") || request.method !== "POST"
          || url.pathname !== `${pathPrefix}/v1/messages` || !["", "?beta=true"].includes(url.search)) {
        return reject(403, "only_synthetic_messages_allowed");
      }
      if (request.headers.get("authorization") !== `Bearer ${SYNTHETIC_TOKEN}`
          || request.headers.has("x-api-key") || request.headers.has("cookie")) return reject(403, "synthetic_auth_required");
      if (![null, "", "identity"].includes(request.headers.get("content-encoding"))) return reject(415, "encoded_body_rejected");
      const declared = request.headers.get("content-length");
      if (declared !== null && (!/^[0-9]+$/.test(declared) || Number(declared) > STUB_BODY_LIMIT)) return reject(413, "synthetic_body_limit");
      if (counts.requests >= MAX_REQUESTS) return reject(429, "synthetic_request_budget");
      let body: unknown;
      let total = 0;
      const reader = request.body?.getReader();
      if (!reader) return reject(400, "synthetic_body_required");
      try {
        const chunks: Uint8Array[] = [];
        while (true) {
          const part = await reader.read();
          if (part.done) break;
          total += part.value.byteLength;
          if (total > STUB_BODY_LIMIT) { await reader.cancel(); return reject(413, "synthetic_body_limit"); }
          chunks.push(part.value);
        }
        const bytes = new Uint8Array(total);
        let offset = 0;
        for (const chunk of chunks) { bytes.set(chunk, offset); offset += chunk.length; }
        body = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
      } catch { return reject(400, "synthetic_body_invalid"); }
      finally { reader.releaseLock(); }
      if (!body || typeof body !== "object" || Array.isArray(body)) return reject(400, "synthetic_body_invalid");
      const input = body as Record<string, unknown>;
      if (input.model !== "claude-sonnet-5" || typeof input.max_tokens !== "number"
          || !Number.isInteger(input.max_tokens) || input.max_tokens < 1 || input.max_tokens > 128
          || input.stream !== undefined && typeof input.stream !== "boolean") return reject(400, "synthetic_contract_rejected");
      // Concurrent body readers must also reserve against the final budget.
      if (stopped) return reject(503, "synthetic_stub_stopped");
      if (counts.requests >= MAX_REQUESTS) return reject(429, "synthetic_request_budget");
      counts.requests++;
      last = { stream: input.stream === true, maxTokens: input.max_tokens, bodyBytes: total, synthetic: true };
      for (const done of [...waiters]) done();
      if (options.failure503) {
        counts.failed++;
        return Response.json({ type: "error", error: { type: "overloaded_error", message: "synthetic_unavailable" } }, { status: 503 });
      }
      if (input.stream !== true) {
        counts.completed++;
        return Response.json({ id: "msg_cli_synthetic", type: "message", role: "assistant", model: "claude-sonnet-5",
          content: [{ type: "text", text: SYNTHETIC_TEXT }], stop_reason: "end_turn", stop_sequence: null,
          usage: { input_tokens: 1, output_tokens: 1, cache_creation_input_tokens: 0, cache_read_input_tokens: 0 } },
        { headers: { "x-probe-synthetic": "true" } });
      }
      counts.active++;
      const lifetime = new AbortController();
      running.add(lifetime);
      let finished = false;
      let controller: ReadableStreamDefaultController<Uint8Array>;
      const finish = (outcome: Outcome) => {
        if (finished) return;
        finished = true; counts.active--; counts[outcome]++;
        request.signal.removeEventListener("abort", aborted);
        lifetime.signal.removeEventListener("abort", failed);
        running.delete(lifetime);
      };
      const failed = () => { finish("cancelled"); try { controller.error(new Error("synthetic_stub_cancelled")); } catch { /* already closed */ } };
      const aborted = () => lifetime.abort();
      const frames = events();
      let index = 0;
      const stream = new ReadableStream<Uint8Array>({
        start(value) { controller = value; },
        async pull(value) {
          try {
            await delay(frameDelay, lifetime.signal);
            if (finished) return;
            value.enqueue(frames[index++]);
            if (index === frames.length) { value.close(); finish("completed"); }
          } catch { finish("cancelled"); try { value.error(new Error("synthetic_stub_cancelled")); } catch { /* already closed */ } }
        },
        cancel() { finish("cancelled"); lifetime.abort(); },
      });
      lifetime.signal.addEventListener("abort", failed, { once: true });
      request.signal.addEventListener("abort", aborted, { once: true });
      if (request.signal.aborted) aborted();
      return new Response(stream, { headers: { "content-type": "text/event-stream", "x-probe-synthetic": "true" } });
    },
    async stop() { stopped = true; for (const lifetime of [...running]) lifetime.abort(); for (const done of [...waiters]) done(); },
  };
}

/** Explicit Linux-lab smoke entry only; no host/public listener or external client. */
export function serveCliStub(options: StubOptions = {}) {
  const stub = createCliStub(options);
  const server = Bun.serve({ hostname: "127.0.0.1", port: 0, development: false,
    maxRequestBodySize: STUB_BODY_LIMIT, idleTimeout: 30,
    fetch: (request) => stub.handle(request),
    error: () => Response.json({ type: "error", error: { type: "api_error", message: "synthetic_stub_failed" } }, { status: 500 }),
  });
  let stopping: Promise<void> | undefined;
  return { baseURL: `http://127.0.0.1:${server.port}${stub.pathPrefix}`, stats: stub.stats,
    waitForRequest: stub.waitForRequest,
    stop: () => stopping ??= stopWithDeadline(() => server.stop(true), () => stub.stop(), 5000) };
}
