import { expect, test } from "bun:test";
import { CliRunner, type CliChild } from "./process";

const base = "http://127.0.0.1:18765/capture-" + "a".repeat(24);
const request = { prompt: "synthetic process test", stream: true };
function fixture() {
  let out!: ReadableStreamDefaultController<Uint8Array>;
  let err!: ReadableStreamDefaultController<Uint8Array>;
  let resolve!: (code: number) => void;
  let ended = false;
  const signals: string[] = [];
  const exit = (code: number, closeStderr = true) => {
    if (ended) return;
    ended = true;
    try { out.close(); } catch { /* a cancelled pipe is already closed */ }
    if (closeStderr) try { err.close(); } catch { /* a cancelled pipe is already closed */ }
    resolve(code);
  };
  const child: CliChild = { pid: 999999,
    stdout: new ReadableStream({ start(controller) { out = controller; } }),
    stderr: new ReadableStream({ start(controller) { err = controller; } }),
    exited: new Promise((value) => { resolve = value; }),
    kill: (signal) => { signals.push(signal); exit(143); },
  };
  const runner = new CliRunner(base, 100, () => child, (_, signal) => { signals.push("group:" + signal); });
  return { child, runner, signals, exit, output: (value: Uint8Array) => out.enqueue(value),
    errorOutput: (value: Uint8Array) => err.enqueue(value) };
}
test("cancellation reaps even before the first downstream pull", async () => {
  const f = fixture();
  const run = f.runner.start(request, new AbortController().signal);
  expect(f.runner.stats().active).toBe(1);
  await run.cancel();
  expect(f.runner.stats().active).toBe(0);
  expect(f.signals).toContain("group:SIGKILL");
  expect(f.signals).not.toContain("group:SIGTERM");
});
test("a reaped child never causes a signal to its now-unowned numeric PGID", async () => {
  const f = fixture();
  const run = f.runner.start(request, new AbortController().signal);
  f.exit(0);
  await f.child.exited;
  await run.cancel();
  expect(f.signals).toEqual([]);
  expect(f.runner.stats().active).toBe(0);
});
test("abort wakes a blocked pipe read and never exposes raw output", async () => {
  const f = fixture();
  const signal = new AbortController();
  const run = f.runner.start(request, signal.signal);
  const pending = run.chunks.next();
  signal.abort();
  await expect(pending).rejects.toMatchObject({ status: 499 });
  expect(f.runner.stats().active).toBe(0);
});
test("cancel wakes stderr even when a reaped leader left its pipe open", async () => {
  const f = fixture();
  const run = f.runner.start(request, new AbortController().signal);
  f.exit(0, false);
  await f.child.exited;
  await run.cancel();
  expect(f.signals).toEqual([]);
  expect(f.runner.stats().active).toBe(0);
});
test("deadline wakes stderr held open after leader exit", async () => {
  const f = fixture();
  const run = f.runner.start(request, new AbortController().signal);
  f.exit(0, false);
  await f.child.exited;
  const pending = run.chunks.next();
  await expect(pending).rejects.toMatchObject({ status: 504 });
  expect(f.signals).toEqual([]);
  expect(f.runner.stats().active).toBe(0);
});
test("deadline independently reaps a stalled downstream", async () => {
  const f = fixture();
  const run = f.runner.start(request, new AbortController().signal);
  await Bun.sleep(150);
  expect(f.runner.stats().active).toBe(0);
  await expect(run.chunks.next()).rejects.toMatchObject({ status: 504 });
});
test("abort between two events in the same pipe chunk cuts off all further output", async () => {
  const f = fixture();
  const signal = new AbortController();
  const run = f.runner.start(request, signal.signal);
  const events = [
    { type: "message_start", message: { id: "msg_synthetic", type: "message", role: "assistant",
      model: "synthetic", content: [], stop_reason: null, stop_sequence: null,
      usage: { input_tokens: 1, output_tokens: 0 } } },
    { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
  ];
  f.output(new TextEncoder().encode(events.map(event => JSON.stringify({ type: "stream_event", event }) + "\n").join("")));
  expect((await run.chunks.next()).done).toBe(false);
  signal.abort();
  await expect(run.chunks.next()).rejects.toMatchObject({ status: 499 });
  expect(f.runner.stats().active).toBe(0);
});
test("one active request, closed runner, and pre-aborted input never spawn another child", async () => {
  const f = fixture();
  f.runner.start(request, new AbortController().signal);
  expect(() => f.runner.start(request, new AbortController().signal)).toThrow();
  expect(f.runner.stats().started).toBe(1);
  await f.runner.close();
  expect(() => f.runner.start(request, new AbortController().signal)).toThrow();
  const g = fixture();
  expect(() => g.runner.start(request, AbortSignal.abort())).toThrow();
  expect(g.runner.stats().started).toBe(0);
});
test("malformed stdout and oversized stderr fail closed with a fixed error", async () => {
  for (const mode of ["stdout", "stderr"]) {
    const f = fixture();
    const run = f.runner.start(request, new AbortController().signal);
    if (mode === "stdout") f.output(new TextEncoder().encode("RAW_PRIVATE_MUST_NOT_ESCAPE\n"));
    else f.errorOutput(new Uint8Array(65537));
    await expect(run.chunks.next()).rejects.toThrow("CLI execution failed");
    expect(f.runner.stats().active).toBe(0);
  }
});
