import { describe, expect, test } from "bun:test";
import { CLI_EVENT_LINE_BYTES, CLI_EVENT_TOTAL_BYTES, CliEventDecoder, CliEventError } from "./events";

const encoder = new TextEncoder();
const utf8 = new TextDecoder("utf-8", { fatal: true });
type Row = Record<string, any>;
const wrap = (event: Row): Row => ({ type: "stream_event", event, parent_tool_use_id: null });
const result = (): Row => ({ type: "result", subtype: "success", is_error: false });
const bytes = (rows: Row[], finalNewline = true): Uint8Array => encoder.encode(rows.map((row) => JSON.stringify(row)).join("\n") + (finalNewline ? "\n" : ""));
const outputText = (chunks: Uint8Array[]): string => chunks.map((chunk) => utf8.decode(chunk)).join("");

function stream(): Row[] {
  return [
    wrap({ type: "message_start", message: { id: "msg_test", type: "message", role: "assistant", model: "test-model",
      content: [], stop_reason: null, stop_sequence: null,
      usage: { input_tokens: 13, output_tokens: 1, cache_read_input_tokens: 4 } } }),
    wrap({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }),
    wrap({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "你好 🌍\nline" } }),
    wrap({ type: "content_block_stop", index: 0 }),
    wrap({ type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 9 } }),
    wrap({ type: "message_stop" }),
    result(),
  ];
}

function rejects(rows: Row[], code?: string): void {
  const decoder = new CliEventDecoder();
  let error: unknown;
  try { decoder.push(bytes(rows)); decoder.finish(0); } catch (caught) { error = caught; }
  expect(error).toBeInstanceOf(CliEventError);
  if (code) expect((error as CliEventError).code).toBe(code);
  expect(() => decoder.message()).toThrow("cli_output_incomplete");
  expect(() => decoder.push(new Uint8Array())).toThrow("cli_output_closed");
}

describe("bounded text-only CLI event decoder", () => {
  test("diagnostics expose only fixed fields and fresh copies", () => {
    const decoder = new CliEventDecoder();
    const initial = { recordType: "none", eventType: "none", systemSubtype: "none", usageExtension: "none" };
    expect(decoder.diagnostics()).toEqual(initial);
    (decoder.diagnostics() as Row).recordType = "DO_NOT_EMIT";
    expect(decoder.diagnostics()).toEqual(initial);
    for (const type of ["assistant", "user", "rate_limit_event"]) {
      decoder.push(bytes([{ type, secret_key_DO_NOT_EMIT: "secret_value_DO_NOT_EMIT" }]));
      expect(decoder.diagnostics()).toEqual({ ...initial, recordType: type });
      expect(JSON.stringify(decoder.diagnostics())).not.toContain("DO_NOT_EMIT");
    }
    decoder.push(bytes([{ type: "system", subtype: "init", secret: "DO_NOT_EMIT" }]));
    expect(decoder.diagnostics()).toEqual({ ...initial, recordType: "system", systemSubtype: "init" });
  });

  test("usage diagnostics classify known rejected extensions but never their values", () => {
    for (const key of ["service_tier", "server_tool_use", "inference_geo", "speed", "secret_key_DO_NOT_EMIT"]) {
      for (const delta of [false, true]) {
        const rows = stream();
        const value = delta ? rows[4]!.event.usage : rows[0]!.event.message.usage;
        value[key] = { secret_key_DO_NOT_EMIT: "secret_value_DO_NOT_EMIT" };
        const decoder = new CliEventDecoder();
        expect(() => decoder.push(bytes(rows))).toThrow("cli_output_unsupported");
        expect(decoder.diagnostics()).toEqual({ recordType: "stream_event",
          eventType: delta ? "message_delta" : "message_start", systemSubtype: "none",
          usageExtension: key.startsWith("secret_") ? "other" : key });
        expect(JSON.stringify(decoder.diagnostics())).not.toContain("DO_NOT_EMIT");
        expect(() => decoder.message()).toThrow("cli_output_incomplete");
      }
    }
    const rows = stream();
    rows[0]!.event.message.usage.cache_creation = { secret_key_DO_NOT_EMIT: "secret_value_DO_NOT_EMIT" };
    const decoder = new CliEventDecoder();
    expect(() => decoder.push(bytes(rows))).toThrow("cli_output_unsupported");
    expect(decoder.diagnostics().usageExtension).toBe("cache_creation");
    expect(JSON.stringify(decoder.diagnostics())).not.toContain("DO_NOT_EMIT");
  });

  test("diagnosing system subtypes does not permit new metadata or leak unknown subtypes", () => {
    for (const subtype of ["hook_started", "hook_progress", "hook_response", "api_retry", "compact_boundary",
      "task_started", "task_progress", "task_notification", "local_command_output", "secret_subtype_DO_NOT_EMIT"]) {
      const decoder = new CliEventDecoder();
      expect(() => decoder.push(bytes([{ type: "system", subtype, private: "secret_value_DO_NOT_EMIT" }]))).toThrow("cli_output_unsupported");
      expect(decoder.diagnostics()).toEqual({ recordType: "system", eventType: "none",
        systemSubtype: subtype.startsWith("secret_") ? "other" : subtype, usageExtension: "none" });
      expect(JSON.stringify(decoder.diagnostics())).not.toContain("DO_NOT_EMIT");
    }
  });

  test("observed system status is ignored before init and within a stream without changing success gates", () => {
    const reference = new CliEventDecoder();
    const expected = [...reference.push(bytes(stream())), ...reference.finish(0)];
    const status = { type: "system", subtype: "status", status: "DO_NOT_EMIT", secret: "DO_NOT_EMIT" };
    const rows = stream();
    rows.splice(3, 0, status);
    rows.splice(1, 0, status);
    rows.unshift(status, { type: "system", subtype: "init" });
    const decoder = new CliEventDecoder();
    const emitted = decoder.push(bytes(rows));
    expect(outputText(emitted)).not.toContain("message_stop");
    expect(outputText(emitted)).not.toContain("DO_NOT_EMIT");
    expect(() => decoder.message()).toThrow("cli_output_incomplete");
    expect([...emitted, ...decoder.finish(0)]).toEqual(expected);
    expect(decoder.message()).toEqual(reference.message());
    rejects([...stream(), status], "cli_output_order");
    rejects([status], "cli_output_incomplete");
    rejects([...stream().slice(0, -1), status], "cli_output_incomplete");
    rejects([{ type: "system", subtype: "unknown", status: "DO_NOT_EMIT" }], "cli_output_unsupported");
  });

  test("unknown records, events and malformed lines cannot become diagnostic payload", () => {
    const cases: [Row, Row][] = [
      [{ type: "secret_type_DO_NOT_EMIT", arbitrary: "DO_NOT_EMIT" }, { recordType: "other", eventType: "none" }],
      [wrap({ type: "secret_event_DO_NOT_EMIT", arbitrary: "DO_NOT_EMIT" }), { recordType: "stream_event", eventType: "other" }],
      [{ type: "stream_event", event: "secret_value_DO_NOT_EMIT" }, { recordType: "stream_event", eventType: "other" }],
      [wrap({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "DO_NOT_EMIT" } }),
        { recordType: "stream_event", eventType: "content_block_delta" }],
    ];
    for (const [row, expected] of cases) {
      const decoder = new CliEventDecoder();
      expect(() => decoder.push(bytes([row]))).toThrow();
      expect(decoder.diagnostics()).toEqual({ ...expected, systemSubtype: "none", usageExtension: "none" });
      expect(JSON.stringify(decoder.diagnostics())).not.toContain("DO_NOT_EMIT");
    }
    const decoder = new CliEventDecoder();
    decoder.push(bytes([{ type: "system", subtype: "init" }]));
    expect(() => decoder.push(encoder.encode("private_invalid_JSON_DO_NOT_EMIT\n"))).toThrow("cli_output_invalid");
    expect(decoder.diagnostics()).toEqual({ recordType: "other", eventType: "none", systemSubtype: "none", usageExtension: "none" });
  });

  test("one-byte chunks preserve UTF-8 and hold message_stop until successful exit", () => {
    const decoder = new CliEventDecoder();
    const output: Uint8Array[] = [];
    for (const byte of bytes(stream())) output.push(...decoder.push(new Uint8Array([byte])));
    expect(outputText(output)).toContain("你好 🌍\\nline");
    expect(outputText(output)).not.toContain("message_stop");
    expect(() => decoder.message()).toThrow("cli_output_incomplete");
    expect(outputText(decoder.finish(0))).toBe('event: message_stop\ndata: {"type":"message_stop"}\n\n');
    expect(decoder.message()).toEqual({ id: "msg_test", type: "message", role: "assistant", model: "test-model",
      content: [{ type: "text", text: "你好 🌍\nline" }], stop_reason: "end_turn", stop_sequence: null,
      usage: { input_tokens: 13, output_tokens: 9, cache_read_input_tokens: 4 } });
    expect(() => decoder.finish(0)).toThrow("cli_output_closed");
  });

  test("CRLF and final unterminated result are accepted, known metadata never leaks", () => {
    const decoder = new CliEventDecoder();
    const rows = [{ type: "system", subtype: "init", secret: "DO_NOT_EMIT" },
      { type: "user", message: "DO_NOT_EMIT" }, ...stream().slice(0, -1),
      { type: "assistant", message: "DO_NOT_EMIT" },
      { type: "rate_limit_event", rate_limit_info: { detail: "DO_NOT_EMIT" } }, result()];
    const input = encoder.encode(utf8.decode(bytes(rows, false)).replaceAll("\n", "\r\n"));
    const output = [...decoder.push(input), ...decoder.finish(0)];
    expect(outputText(output)).not.toContain("DO_NOT_EMIT");
    expect(outputText(output).match(/event: message_stop/g)).toHaveLength(1);
  });

  test("normalizes events without echoing extra CLI or API metadata", () => {
    const rows = stream();
    for (const row of rows) {
      row.private = "DO_NOT_EMIT";
      if (row.event) row.event.private = "DO_NOT_EMIT";
    }
    rows[0]!.event.message.private = "DO_NOT_EMIT";
    const decoder = new CliEventDecoder();
    const output = [...decoder.push(bytes(rows)), ...decoder.finish(0)];
    expect(outputText(output)).not.toContain("DO_NOT_EMIT");
    const value = decoder.message();
    (value.content as Row[])[0]!.text = "mutated";
    (value.usage as Row).input_tokens = -1;
    expect((decoder.message().content as Row[])[0]!.text).toBe("你好 🌍\nline");
    expect((decoder.message().usage as Row).input_tokens).toBe(13);
  });

  test("supports sequential text blocks, ping, and cumulative usage updates", () => {
    const rows = stream();
    rows.splice(4, 0, wrap({ type: "ping" }),
      wrap({ type: "content_block_start", index: 1, content_block: { type: "text", text: "second" } }),
      wrap({ type: "content_block_stop", index: 1 }),
      wrap({ type: "message_delta", delta: { stop_reason: null, stop_sequence: null }, usage: { output_tokens: 7 } }));
    rows[0]!.event.message.usage.cache_creation = { ephemeral_5m_input_tokens: 2, ephemeral_1h_input_tokens: 0 };
    const decoder = new CliEventDecoder();
    const output = [...decoder.push(bytes(rows)), ...decoder.finish(0)];
    expect(outputText(output)).toContain('event: ping\ndata: {"type":"ping"}');
    expect(decoder.message().content).toEqual([{ type: "text", text: "你好 🌍\nline" }, { type: "text", text: "second" }]);
    expect(decoder.message().usage).toEqual({ input_tokens: 13, output_tokens: 9, cache_read_input_tokens: 4,
      cache_creation: { ephemeral_5m_input_tokens: 2, ephemeral_1h_input_tokens: 0 } });
  });

  test("nonzero exit never releases cached message_stop even after success result", () => {
    for (const exitCode of [1, -1, NaN, 0.5]) {
      const decoder = new CliEventDecoder();
      expect(outputText(decoder.push(bytes(stream())))).not.toContain("message_stop");
      expect(() => decoder.finish(exitCode)).toThrow("cli_output_failed");
      expect(() => decoder.message()).toThrow("cli_output_incomplete");
    }
  });

  test("requires explicit successful result after complete message and forbids trailing records", () => {
    for (const terminal of [undefined, { type: "result", subtype: "error_during_execution", is_error: true },
      { type: "result", subtype: "success", is_error: true }, { type: "result", subtype: "success" },
      { type: "result", subtype: "success", is_error: "false" }]) {
      const rows = stream().slice(0, -1);
      if (terminal) rows.push(terminal);
      rejects(rows);
    }
    rejects([result()], "cli_output_order");
    rejects([...stream(), result()], "cli_output_order");
    rejects([...stream(), { type: "assistant" }], "cli_output_order");
    rejects(stream().slice(0, -2), "cli_output_incomplete");
  });

  test("rejects illegal message and content order or indexes", () => {
    const mutations: ((rows: Row[]) => void)[] = [
      (rows) => rows.splice(0, 1),
      (rows) => rows.splice(1, 0, rows[0]!),
      (rows) => rows.splice(1, 1),
      (rows) => { rows[1]!.event.index = 1; },
      (rows) => { rows[2]!.event.index = 1; },
      (rows) => { rows[3]!.event.index = 1; },
      (rows) => rows.splice(3, 1),
      (rows) => rows.splice(4, 1),
      (rows) => rows.splice(5, 0, rows[1]!),
      (rows) => rows.splice(6, 0, wrap({ type: "ping" })),
      (rows) => rows.splice(2, 0, { type: "system", subtype: "init" }),
    ];
    for (const mutation of mutations) { const rows = stream(); mutation(rows); rejects(rows, "cli_output_order"); }
  });

  test("fails closed for tool, thinking, unknown, child, or error events", () => {
    for (const kind of ["tool_use", "thinking", "redacted_thinking"]) {
      const rows = stream(); rows[1]!.event.content_block.type = kind; rejects(rows, "cli_output_unsupported");
    }
    for (const kind of ["thinking_delta", "input_json_delta", "signature_delta", "citations_delta"]) {
      const rows = stream(); rows[2]!.event.delta.type = kind; rejects(rows, "cli_output_unsupported");
    }
    const child = stream(); child[0]!.parent_tool_use_id = "tool_secret"; rejects(child, "cli_output_unsupported");
    const toolStop = stream(); toolStop[4]!.event.delta.stop_reason = "tool_use"; rejects(toolStop, "cli_output_unsupported");
    rejects([wrap({ type: "error", error: { message: "DO_NOT_EMIT" } })], "cli_output_failed");
    rejects([{ type: "unknown", data: "DO_NOT_EMIT" }], "cli_output_unsupported");
    rejects([{ type: "system", subtype: "unknown" }], "cli_output_unsupported");
  });

  test("requires actual nonnegative safe-integer token counts and never invents missing ones", () => {
    for (const invalid of [-1, 1.5, "1", true, null, Number.MAX_SAFE_INTEGER + 1]) {
      const rows = stream(); rows[0]!.event.message.usage.input_tokens = invalid; rejects(rows, "cli_output_invalid");
      const delta = stream(); delta[4]!.event.usage.output_tokens = invalid; rejects(delta, "cli_output_invalid");
    }
    const missing = stream(); delete missing[0]!.event.message.usage.input_tokens; rejects(missing, "cli_output_invalid");
    const missingDelta = stream(); delete missingDelta[4]!.event.usage.output_tokens; rejects(missingDelta, "cli_output_invalid");
    const decreasing = stream(); decreasing[4]!.event.usage.output_tokens = 0; rejects(decreasing, "cli_output_invalid");
    const unsupported = stream(); unsupported[0]!.event.message.usage.mystery = 2; rejects(unsupported, "cli_output_unsupported");
    const nested = stream(); nested[0]!.event.message.usage.cache_creation = { ephemeral_5m_input_tokens: -1 }; rejects(nested, "cli_output_invalid");
  });

  test("invalid JSON and UTF-8 have fixed errors without raw bytes or parse diagnostics", () => {
    for (const input of [encoder.encode('DO_NOT_EMIT\n'), encoder.encode('{"private":"DO_NOT_EMIT"\n'),
      encoder.encode("null\n"), encoder.encode("[]\n"), new Uint8Array([0xff, 10]),
      new Uint8Array([0xc0, 0xaf, 10]), new Uint8Array([0xe4, 10])]) {
      const decoder = new CliEventDecoder();
      expect(() => decoder.push(input)).toThrow("cli_output_invalid");
      expect(() => decoder.finish(0)).toThrow("cli_output_closed");
    }
    const truncated = new CliEventDecoder();
    truncated.push(new Uint8Array([0xe4]));
    expect(() => truncated.finish(0)).toThrow("cli_output_invalid");
  });

  test("enforces exact raw line and total byte bounds before decoding or allocation", () => {
    const prefix = '{"type":"assistant","padding":"';
    const line = prefix + "x".repeat(CLI_EVENT_LINE_BYTES - prefix.length - 2) + '"}';
    expect(encoder.encode(line).length).toBe(CLI_EVENT_LINE_BYTES);
    const decoder = new CliEventDecoder();
    expect(decoder.push(encoder.encode(line + "\n"))).toEqual([]);
    const oversized = new CliEventDecoder();
    oversized.push(encoder.encode(line));
    expect(() => oversized.push(encoder.encode("x"))).toThrow("cli_output_limit");
    const onePush = new CliEventDecoder();
    expect(() => onePush.push(new Uint8Array(CLI_EVENT_TOTAL_BYTES + 1))).toThrow("cli_output_limit");
    const total = new CliEventDecoder();
    const boundedLine = encoder.encode(line.slice(0, -3) + '"}\n');
    expect(boundedLine.length).toBe(CLI_EVENT_LINE_BYTES);
    for (let i = 0; i < CLI_EVENT_TOTAL_BYTES / boundedLine.length; i++) expect(total.push(boundedLine)).toEqual([]);
    expect(() => total.push(encoder.encode("\n"))).toThrow("cli_output_limit");
  });

  test("missing fields and incomplete active blocks cannot produce a successful message", () => {
    for (const field of ["id", "model", "usage", "stop_reason", "stop_sequence"]) {
      const rows = stream(); delete rows[0]!.event.message[field]; rejects(rows, "cli_output_invalid");
    }
    const rows = stream(); rows[0]!.event.message.content = [{ type: "text", text: "unexpected" }]; rejects(rows, "cli_output_invalid");
    const decoder = new CliEventDecoder();
    decoder.push(bytes(stream().slice(0, 3)));
    expect(() => decoder.finish(0)).toThrow("cli_output_incomplete");
  });
});
