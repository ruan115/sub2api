import { waitForDelay } from "./cancellation";
import { throwIfAborted, TurnError } from "./errors";
import type { TurnCompletion, TurnEngine, TurnHandle, TurnRequest } from "./types";

export interface FakeTurnOptions {
  readonly responseText?: string;
  readonly chunkBytes?: number;
  readonly delayMs?: number;
  readonly maxActiveTurns?: number;
  readonly scenario?: "success" | "upstream-error" | "failure-after-first-chunk";
}

function boundedInteger(value: number, min: number, max: number, name: string): number {
  if (!Number.isInteger(value) || value < min || value > max) {
    throw new Error(`Invalid fake ${name}`);
  }
  return value;
}

/** Synthetic output only. No environment, filesystem, CLI or network access. */
export class FakeTurnEngine implements TurnEngine {
  readonly mode = "fake" as const;
  private readonly active = new Set<TurnHandle>();
  private readonly text: string;
  private readonly chunkBytes: number;
  private readonly delayMs: number;
  private readonly maxActive: number;
  private readonly scenario: NonNullable<FakeTurnOptions["scenario"]>;
  private closed = false;
  private counters = { started: 0, completed: 0, cancelled: 0, failed: 0, producedChunks: 0 };

  constructor(options: FakeTurnOptions = {}) {
    this.text = options.responseText ?? "Synthetic fake response: 你好 🌍";
    if (typeof this.text !== "string" || new TextEncoder().encode(this.text).length > 8192) {
      throw new Error("Fake response text exceeds its 8192-byte limit");
    }
    this.chunkBytes = boundedInteger(options.chunkBytes ?? 128, 1, 65536, "chunk size");
    this.delayMs = boundedInteger(options.delayMs ?? 5, 0, 1000, "delay");
    this.maxActive = boundedInteger(options.maxActiveTurns ?? 4, 1, 64, "capacity");
    this.scenario = options.scenario ?? "success";
    if (!["success", "upstream-error", "failure-after-first-chunk"].includes(this.scenario)) {
      throw new Error("Unknown fake scenario");
    }
  }

  stats() {
    return { ...this.counters, active: this.active.size, closed: this.closed };
  }

  async start(request: TurnRequest, signal: AbortSignal): Promise<TurnHandle> {
    throwIfAborted(signal);
    if (this.closed) throw new TurnError(503, "unavailable_error", "Fake engine is closed");
    if (this.active.size >= this.maxActive) {
      throw new TurnError(429, "rate_limit_error", "Fake engine capacity exhausted");
    }
    const controller = new AbortController();
    const sequence = ++this.counters.started;
    let settled = false;
    let resolveDone!: (value: TurnCompletion) => void;
    const done = new Promise<TurnCompletion>((resolve) => { resolveDone = resolve; });
    const finish = (outcome: TurnCompletion) => {
      if (settled) return;
      settled = true;
      signal.removeEventListener("abort", abort);
      this.active.delete(handle);
      this.counters[outcome]++;
      resolveDone(outcome);
    };
    const abort = () => {
      controller.abort();
      finish("cancelled");
    };
    const engine = this;
    async function* generate(): AsyncGenerator<Uint8Array> {
      let outcome: TurnCompletion = "completed";
      let emitted = 0;
      try {
        throwIfAborted(controller.signal);
        for (const fragment of engine.fragments(request, sequence)) {
          const bytes = new TextEncoder().encode(fragment);
          for (let offset = 0; offset < bytes.length; offset += engine.chunkBytes) {
            await waitForDelay(engine.delayMs, controller.signal);
            if (engine.scenario === "failure-after-first-chunk" && emitted > 0) {
              throw new TurnError(502, "api_error", "Synthetic stream failure");
            }
            throwIfAborted(controller.signal);
            emitted++;
            engine.counters.producedChunks++;
            yield bytes.subarray(offset, offset + engine.chunkBytes);
          }
        }
      } catch (error) {
        outcome = controller.signal.aborted ? "cancelled" : "failed";
        throw error;
      } finally {
        finish(outcome);
      }
    }
    const iterator = generate();
    const isError = this.scenario === "upstream-error";
    const handle: TurnHandle = {
      start: {
        status: isError ? 503 : 200,
        statusText: isError ? "Service Unavailable" : "OK",
        headers: {
          "content-type": request.stream && !isError ? "text/event-stream" : "application/json",
          "cache-control": "no-store",
          "x-isthmus-runtime": "fake",
        },
      },
      chunks: iterator,
      done,
      cancel: async () => {
        if (!settled) abort();
        try { await iterator.return(undefined); } catch { /* cancellation owns cleanup */ }
      },
    };
    this.active.add(handle);
    signal.addEventListener("abort", abort, { once: true });
    if (signal.aborted) abort();
    return handle;
  }

  private *fragments(request: TurnRequest, sequence: number): Generator<string> {
    if (this.scenario === "upstream-error") {
      yield JSON.stringify({ type: "error", error: { type: "overloaded_error", message: "Synthetic upstream unavailable" } });
      return;
    }
    const message = {
      id: `msg_fake_${sequence.toString().padStart(4, "0")}`,
      type: "message", role: "assistant", model: request.model,
      content: [{ type: "text", text: this.text }],
      stop_reason: "end_turn", stop_sequence: null,
      usage: { input_tokens: 0, output_tokens: 0 },
    };
    if (!request.stream) {
      yield JSON.stringify(message);
      return;
    }
    const event = (type: string, data: object) => `event: ${type}\ndata: ${JSON.stringify(data)}\n\n`;
    yield event("message_start", { type: "message_start", message: { ...message, content: [], stop_reason: null } });
    yield event("content_block_start", { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } });
    yield event("content_block_delta", { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: this.text } });
    yield event("content_block_stop", { type: "content_block_stop", index: 0 });
    yield event("message_delta", { type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 0 } });
    yield event("message_stop", { type: "message_stop" });
  }

  async close(): Promise<void> {
    this.closed = true;
    await Promise.all([...this.active].map((turn) => turn.cancel()));
  }
}
