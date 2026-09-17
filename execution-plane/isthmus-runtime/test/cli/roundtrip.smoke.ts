// Explicit Linux lab only. `bun test` may import this file but never starts it.
// Requires the outer reviewed network=none, UID1000, read-only/capless container.
import assert from "node:assert/strict";
import { readdir } from "node:fs/promises";
import { serveCliProbe } from "../../src/app/cli/serve";
import { serveCliStub, SYNTHETIC_TEXT, type StubOptions } from "./stub";

type App = ReturnType<typeof serveCliProbe>;
type Stub = ReturnType<typeof serveCliStub>;
const pause = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));
const body = (stream: boolean) => JSON.stringify({ model: "claude-sonnet-5", max_tokens: 128, stream,
  messages: [{ role: "user", content: "Synthetic isolated test only. Do not use tools. Reply CLI_ROUNDTRIP_OK." }] });

function request(app: App, stream: boolean, signal = AbortSignal.timeout(15_000)) {
  return fetch(`${app.url}/v1/messages`, { method: "POST", headers: { "content-type": "application/json" },
    body: body(stream), signal });
}

async function idle(app: App, stub: Stub) {
  const deadline = performance.now() + 5000;
  while (app.stats().active !== 0 || stub.stats().active !== 0) {
    if (performance.now() >= deadline) throw new Error("owned_execution_did_not_finish");
    await pause(20);
  }
  assert.equal(app.stats().active, 0);
  assert.equal(stub.stats().active, 0);
}

async function fixture(options: StubOptions, timeoutMs: number,
    run: (app: App, stub: Stub) => Promise<void>) {
  const stub = serveCliStub(options);
  let app: App | undefined;
  try {
    app = serveCliProbe({ baseURL: stub.baseURL, timeoutMs });
    await run(app, stub);
  } catch (error) {
    // Fixed process-stage/error enums and numeric counts, never raw CLI output.
    console.error(JSON.stringify({ diagnostic: "cli-roundtrip-failure", execution: app?.diagnostics(),
      app: app?.stats(), stub: stub.stats() }));
    throw error;
  } finally {
    // Try both owned listener shutdowns even when the first one fails.
    const outcomes = await Promise.allSettled([app?.stop(), stub.stop()]);
    assert.ok(outcomes.every((result) => result.status === "fulfilled"), "owned_listener_shutdown_failed");
    if (app) assert.deepEqual(app.stats(), { active: 0, started: app.stats().started, closed: true });
    assert.equal(stub.stats().active, 0);
  }
}

async function readEvents(response: Response, onEvent?: (event: Record<string, any>) => void) {
  const reader = response.body!.getReader();
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let pending = "", total = 0;
  const events: Record<string, any>[] = [];
  try {
    while (true) {
      const part = await reader.read();
      if (part.done) break;
      total += part.value.byteLength;
      assert.ok(total <= 256 * 1024, "synthetic_response_limit");
      pending += decoder.decode(part.value, { stream: true });
      let boundary: number;
      while ((boundary = pending.indexOf("\n\n")) !== -1) {
        const frame = pending.slice(0, boundary);
        pending = pending.slice(boundary + 2);
        const line = frame.split("\n").find((item) => item.startsWith("data: "));
        if (!line) continue;
        const event = JSON.parse(line.slice(6));
        events.push(event); onEvent?.(event);
      }
    }
    pending += decoder.decode();
    assert.equal(pending.trim(), "", "truncated_synthetic_sse");
    return events;
  } finally { reader.releaseLock(); }
}

export async function runCliRoundtripSmoke() {
  assert.equal(process.platform, "linux", "explicit_linux_lab_required");
  assert.equal(process.getuid?.(), 1000, "explicit_nonroot_lab_user_required");
  assert.deepEqual((await readdir("/sys/class/net")).sort(), ["lo"], "network_none_required");

  await fixture({}, 30_000, async (app, stub) => {
    const health = await (await fetch(`${app.url}/health`, { signal: AbortSignal.timeout(3000) })).json();
    assert.equal(health.mode, "cli-probe");
    assert.equal(health.real_model_requests, 0);
    const response = await request(app, false);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("x-isthmus-usage"), "synthetic");
    const message = await response.json();
    assert.equal(message.type, "message");
    assert.equal(message.role, "assistant");
    assert.equal(message.content.map((item: { text?: string }) => item.text ?? "").join(""), SYNTHETIC_TEXT);
    assert.equal(message.stop_reason, "end_turn");
    assert.equal(message.usage.input_tokens, 1);
    assert.equal(message.usage.output_tokens, 1);
    await idle(app, stub);
    assert.equal(app.stats().started, 1);
    assert.equal(stub.stats().requests, 1);
    assert.equal(stub.stats().completed, 1);
    console.log("cli-roundtrip JSON PASS: synthetic usage, one CLI reaped");
  });

  await fixture({ frameDelayMs: 60 }, 30_000, async (app, stub) => {
    const response = await request(app, true);
    assert.equal(response.status, 200);
    assert.equal(response.headers.get("content-type"), "text/event-stream");
    const events = await readEvents(response, (event) => {
      if (event.type === "message_stop") assert.equal(app.stats().active, 0, "message_stop_must_follow_cli_reap");
    });
    assert.equal(events[0].type, "message_start");
    assert.equal(events.at(-1)?.type, "message_stop");
    assert.equal(events.filter((event) => event.type === "message_stop").length, 1);
    assert.equal(events.filter((event) => event.type === "content_block_delta").map((event) => event.delta.text).join(""), SYNTHETIC_TEXT);
    assert.equal(events[0].message.usage.input_tokens, 1);
    assert.equal(events.find((event) => event.type === "message_delta")?.usage.output_tokens, 1);
    assert.ok(!events.some((event) => event.type === "error"));
    await idle(app, stub);
    assert.equal(stub.stats().requests, 1);
    console.log("cli-roundtrip SSE PASS: synthetic text/usage, terminal only after CLI exit");
  });

  await fixture({ frameDelayMs: 30_000 }, 30_000, async (app, stub) => {
    const cancellation = new AbortController();
    const pending = request(app, false, cancellation.signal).then(
      (response) => ({ response }), () => ({ response: undefined }));
    await stub.waitForRequest(10_000);
    assert.equal(stub.stats().active, 1, "cancel_phase_must_reach_upstream");
    assert.equal(app.stats().active, 1);
    cancellation.abort();
    const outcome = await pending;
    if (outcome.response) {
      assert.equal(outcome.response.status, 499);
      await outcome.response.arrayBuffer();
    }
    await idle(app, stub);
    assert.equal(stub.stats().completed, 0);
    console.log("cli-roundtrip cancel PASS: reached stub, no completed response, active=0");
  });

  await fixture({ frameDelayMs: 30_000 }, 2500, async (app, stub) => {
    // Attach both settlements before waiting for upstream. If startup misses
    // that gate, fixture cleanup must not leave an unhandled fetch rejection.
    const pending = request(app, false).then(
      (response) => ({ response }), () => ({ response: undefined }));
    await stub.waitForRequest(2400);
    assert.equal(stub.stats().active, 1, "timeout_phase_must_reach_upstream");
    const outcome = await pending;
    assert.ok(outcome.response, "timeout_must_return_fixed_http_error");
    const response = outcome.response;
    assert.equal(response.status, 504);
    const error = await response.json();
    assert.equal(error.type, "error");
    assert.equal(JSON.stringify(error).includes(SYNTHETIC_TEXT), false);
    await idle(app, stub);
    assert.equal(stub.stats().completed, 0);
    console.log("cli-roundtrip timeout PASS: reached stub, HTTP504, active=0");
  });

  await fixture({ failure503: true }, 30_000, async (app, stub) => {
    const response = await request(app, false);
    assert.equal(response.status, 502);
    assert.equal((await response.json()).type, "error");
    await idle(app, stub);
    assert.equal(stub.stats().requests, 1, "503 must not be silently retried");
    assert.equal(stub.stats().completed, 0);
    console.log("cli-roundtrip upstream503 PASS: no retry or fabricated success, active=0");
  });
}

if (import.meta.main) {
  const watchdog = setTimeout(() => {
    console.error("cli-roundtrip smoke FAIL: overall lifecycle deadline"); process.exit(2);
  }, 120_000);
  try {
    await runCliRoundtripSmoke();
    console.log("cli-roundtrip smoke PASS: synthetic only, real_model_requests=0, owned listeners stopped");
  } catch {
    // No arbitrary stdout, stderr, response, prompt, URL or exception is printed.
    console.error("cli-roundtrip smoke FAIL: fixed contract or cleanup gate failed");
    process.exitCode = 1;
  } finally { clearTimeout(watchdog); }
}
