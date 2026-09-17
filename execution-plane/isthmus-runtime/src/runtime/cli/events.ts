/** Bounded, text-only CLI stream-json decoding. No I/O or raw-error logging. */
export const CLI_EVENT_LINE_BYTES = 64 * 1024;
export const CLI_EVENT_TOTAL_BYTES = 2 * 1024 * 1024;

type ErrorCode = "cli_output_invalid" | "cli_output_limit" | "cli_output_order"
  | "cli_output_unsupported" | "cli_output_failed" | "cli_output_incomplete" | "cli_output_closed";

export class CliEventError extends Error {
  constructor(readonly code: ErrorCode) {
    super(code);
    this.name = "CliEventError";
  }
}

type Document = Record<string, unknown>;
type Usage = Record<string, number | Record<string, number>>;
type Phase = "idle" | "content" | "delta" | "stopped" | "result" | "finished" | "failed";
const encoder = new TextEncoder();
const usageCounters = new Set(["input_tokens", "output_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"]);
const cacheCounters = new Set(["ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens"]);
const recordTypes = ["system", "assistant", "user", "rate_limit_event", "result", "stream_event"] as const;
const eventTypes = ["message_start", "content_block_start", "content_block_delta", "content_block_stop",
  "message_delta", "message_stop", "ping", "error"] as const;
const systemSubtypes = ["init", "hook_started", "hook_progress", "hook_response", "status", "api_retry",
  "compact_boundary", "task_started", "task_progress", "task_notification", "local_command_output"] as const;
const usageExtensions = ["service_tier", "server_tool_use", "inference_geo", "speed", "cache_creation"] as const;
type Classified<T extends readonly string[]> = T[number] | "none" | "other";
export interface CliEventDiagnostics {
  recordType: Classified<typeof recordTypes>;
  eventType: Classified<typeof eventTypes>;
  systemSubtype: Classified<typeof systemSubtypes>;
  usageExtension: Classified<typeof usageExtensions>;
}

function classify<T extends string>(value: unknown, choices: readonly T[]): T | "other" {
  // Return only a literal from our own list, never the untrusted input value.
  return choices.find((choice) => choice === value) ?? "other";
}

function emptyDiagnostics(): CliEventDiagnostics {
  return { recordType: "none", eventType: "none", systemSubtype: "none", usageExtension: "none" };
}

function fail(code: ErrorCode): never { throw new CliEventError(code); }

function object(value: unknown): Document {
  if (value === null || typeof value !== "object" || Array.isArray(value)) fail("cli_output_invalid");
  return value as Document;
}

function integer(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) fail("cli_output_invalid");
  return value;
}

function text(value: unknown): string {
  if (typeof value !== "string") fail("cli_output_invalid");
  return value;
}

function usage(value: unknown, required: readonly string[], unsupported: (key: string) => void): Usage {
  const source = object(value);
  const result: Usage = {};
  for (const key of required) if (!Object.hasOwn(source, key)) fail("cli_output_invalid");
  for (const [key, count] of Object.entries(source)) {
    if (usageCounters.has(key)) result[key] = integer(count);
    else if (key === "cache_creation") {
      const cache: Record<string, number> = {};
      for (const [name, tokens] of Object.entries(object(count))) {
        if (!cacheCounters.has(name)) { unsupported("cache_creation"); fail("cli_output_unsupported"); }
        cache[name] = integer(tokens);
      }
      result[key] = cache;
    } else { unsupported(key); fail("cli_output_unsupported"); }
  }
  return result;
}

function mergeUsage(previous: Usage, update: Usage): Usage {
  const merged = { ...previous };
  for (const [key, value] of Object.entries(update)) {
    const old = previous[key];
    if (typeof value === "number") {
      if (typeof old === "number" && value < old) fail("cli_output_invalid");
      merged[key] = value;
    } else {
      const nested = typeof old === "object" ? { ...old } : {};
      for (const [name, count] of Object.entries(value)) {
        if (Object.hasOwn(nested, name) && count < nested[name]!) fail("cli_output_invalid");
        nested[name] = count;
      }
      merged[key] = nested;
    }
  }
  return merged;
}

function sse(event: Document): Uint8Array {
  return encoder.encode(`event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`);
}

/**
 * push emits validated text SSE events, never the terminal message_stop.
 * Only finish(0), after an explicit successful CLI result, releases that stop.
 * A failure poisons the decoder; message() is available only after success.
 */
export class CliEventDecoder {
  private readonly pending = new Uint8Array(CLI_EVENT_LINE_BYTES);
  private readonly decoder = new TextDecoder("utf-8", { fatal: true });
  private pendingBytes = 0;
  private totalBytes = 0;
  private phase: Phase = "idle";
  private active: number | null = null;
  private header: Document | undefined;
  private readonly blocks: { type: "text"; text: string }[] = [];
  private counts: Usage = {};
  private stopReason: string | null = null;
  private stopSequence: string | null = null;
  private detail = emptyDiagnostics();

  /** Fixed schema classifications only: never raw records, arbitrary keys or values. */
  diagnostics(): CliEventDiagnostics { return { ...this.detail }; }

  private readonly unsupportedUsage = (key: string): void => {
    this.detail.usageExtension = classify(key, usageExtensions);
  };

  push(chunk: Uint8Array): Uint8Array[] {
    this.open();
    try {
      if (!(chunk instanceof Uint8Array)) fail("cli_output_invalid");
      if (chunk.byteLength > CLI_EVENT_TOTAL_BYTES - this.totalBytes) fail("cli_output_limit");
      this.totalBytes += chunk.byteLength;
      const output: Uint8Array[] = [];
      let offset = 0;
      while (offset < chunk.byteLength) {
        const newline = chunk.indexOf(10, offset);
        const end = newline < 0 ? chunk.byteLength : newline;
        const length = end - offset;
        if (length > CLI_EVENT_LINE_BYTES - this.pendingBytes) fail("cli_output_limit");
        this.pending.set(chunk.subarray(offset, end), this.pendingBytes);
        this.pendingBytes += length;
        if (newline >= 0) {
          output.push(...this.line());
          offset = newline + 1;
        } else offset = end;
      }
      return output;
    } catch (error) { return this.reject(error); }
  }

  finish(exitCode: number): Uint8Array[] {
    this.open();
    try {
      if (!Number.isInteger(exitCode) || exitCode !== 0) fail("cli_output_failed");
      const output = this.pendingBytes ? this.line() : [];
      if (this.phase !== "result") fail("cli_output_incomplete");
      this.phase = "finished";
      output.push(sse({ type: "message_stop" }));
      return output;
    } catch (error) { return this.reject(error); }
  }

  message(): Record<string, unknown> {
    if (this.phase !== "finished" || !this.header) fail("cli_output_incomplete");
    // Fresh output prevents callers from mutating the decoder's validated state.
    return JSON.parse(JSON.stringify({ ...this.header, content: this.blocks,
      stop_reason: this.stopReason, stop_sequence: this.stopSequence, usage: this.counts }));
  }

  private open(): void {
    if (this.phase === "failed" || this.phase === "finished") fail("cli_output_closed");
  }

  private reject(error: unknown): never {
    this.phase = "failed";
    this.pendingBytes = 0;
    throw error instanceof CliEventError ? error : new CliEventError("cli_output_invalid");
  }

  private line(): Uint8Array[] {
    this.detail = emptyDiagnostics();
    this.detail.recordType = "other";
    let length = this.pendingBytes;
    if (length && this.pending[length - 1] === 13) length--;
    const line = this.decoder.decode(this.pending.subarray(0, length));
    this.pendingBytes = 0;
    if (!line.trim()) return [];
    const value = object(JSON.parse(line));
    this.detail.recordType = classify(value.type, recordTypes);
    if (value.type === "system") this.detail.systemSubtype = classify(value.subtype, systemSubtypes);
    if (value.type === "stream_event") {
      const event = value.event;
      this.detail.eventType = classify(event && typeof event === "object" && !Array.isArray(event)
        ? (event as Document).type : undefined, eventTypes);
    }
    if (this.phase === "result") fail("cli_output_order");
    if (value.type === "system" && value.subtype === "status") return [];
    if (value.type === "system" && value.subtype === "init") {
      if (this.phase !== "idle") fail("cli_output_order");
      return [];
    }
    if (value.type === "assistant" || value.type === "user" || value.type === "rate_limit_event") return [];
    if (value.type === "result") {
      if (value.subtype !== "success" || value.is_error !== false) fail("cli_output_failed");
      if (this.phase !== "stopped") fail("cli_output_order");
      this.phase = "result";
      return [];
    }
    if (value.type !== "stream_event" || (value.parent_tool_use_id !== undefined && value.parent_tool_use_id !== null)) {
      fail("cli_output_unsupported");
    }
    return this.event(object(value.event));
  }

  private event(event: Document): Uint8Array[] {
    const type = event.type;
    if (this.phase === "stopped") fail("cli_output_order");
    if (type === "error") fail("cli_output_failed");
    if (type === "ping") return [sse({ type })];
    if (type === "message_start") {
      if (this.phase !== "idle") fail("cli_output_order");
      const message = object(event.message);
      if (message.type !== "message" || message.role !== "assistant"
          || !Array.isArray(message.content) || message.content.length !== 0
          || message.stop_reason !== null || message.stop_sequence !== null) fail("cli_output_invalid");
      const id = text(message.id), model = text(message.model);
      if (!id || !model) fail("cli_output_invalid");
      this.counts = usage(message.usage, ["input_tokens", "output_tokens"], this.unsupportedUsage);
      this.header = { id, type: "message", role: "assistant", model };
      this.phase = "content";
      return [sse({ type, message: { ...this.header, content: [], stop_reason: null,
        stop_sequence: null, usage: this.counts } })];
    }
    if (type === "content_block_start") {
      if (this.phase !== "content" || this.active !== null) fail("cli_output_order");
      const index = integer(event.index), block = object(event.content_block);
      if (index !== this.blocks.length) fail("cli_output_order");
      if (block.type !== "text" || Object.keys(block).some((key) => key !== "type" && key !== "text")) {
        fail("cli_output_unsupported");
      }
      const content = { type: "text" as const, text: text(block.text) };
      this.blocks.push(content);
      this.active = index;
      return [sse({ type, index, content_block: content })];
    }
    if (type === "content_block_delta" || type === "content_block_stop") {
      const index = integer(event.index);
      if (this.phase !== "content" || this.active === null || index !== this.active) fail("cli_output_order");
      if (type === "content_block_stop") {
        this.active = null;
        return [sse({ type, index })];
      }
      const delta = object(event.delta);
      if (delta.type !== "text_delta" || Object.keys(delta).some((key) => key !== "type" && key !== "text")) {
        fail("cli_output_unsupported");
      }
      const fragment = text(delta.text);
      this.blocks[index]!.text += fragment;
      return [sse({ type, index, delta: { type: "text_delta", text: fragment } })];
    }
    if (type === "message_delta") {
      if ((this.phase !== "content" && this.phase !== "delta") || this.active !== null || !this.blocks.length) {
        fail("cli_output_order");
      }
      const delta = object(event.delta);
      if (!Object.hasOwn(delta, "stop_reason") || !Object.hasOwn(delta, "stop_sequence")) fail("cli_output_invalid");
      if (delta.stop_reason !== null && !["end_turn", "max_tokens", "stop_sequence"].includes(text(delta.stop_reason))) {
        fail("cli_output_unsupported");
      }
      if (delta.stop_sequence !== null && typeof delta.stop_sequence !== "string") fail("cli_output_invalid");
      if ((this.stopReason !== null && delta.stop_reason !== this.stopReason)
          || (delta.stop_reason === "stop_sequence" ? delta.stop_sequence === null : delta.stop_sequence !== null)) {
        fail("cli_output_order");
      }
      const update = usage(event.usage, ["output_tokens"], this.unsupportedUsage);
      this.counts = mergeUsage(this.counts, update);
      this.stopReason = delta.stop_reason as string | null;
      this.stopSequence = delta.stop_sequence as string | null;
      this.phase = "delta";
      return [sse({ type, delta: { stop_reason: this.stopReason, stop_sequence: this.stopSequence }, usage: update })];
    }
    if (type === "message_stop") {
      if (this.phase !== "delta" || this.stopReason === null || this.active !== null) fail("cli_output_order");
      this.phase = "stopped";
      return [];
    }
    return fail("cli_output_unsupported");
  }
}
