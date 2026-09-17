import { CliRunner } from "../../runtime/cli/process";
import { parseCliRequest } from "../../runtime/cli/config";
import { TurnError } from "../../runtime/turn/errors";
import { enforceLocalAccess } from "../../transport/http/local-access";
import { readRequestBodyWithDeadline } from "../../transport/shared/body";
import { stopWithDeadline } from "../shutdown";

const headers = { "x-isthmus-runtime": "cli-probe", "x-isthmus-usage": "synthetic",
  "cache-control": "no-store" };
function errorResponse(error: unknown) {
  const status = error instanceof TurnError ? error.status : 502;
  // Never return arbitrary CLI stderr, paths, output or error messages.
  return Response.json({ type: "error", error: { type: "cli_probe_error", message: "CLI probe request failed" } },
    { status, headers });
}

/** Opt-in test entry only. No listener on import, credentials, or public bind. */
export function serveCliProbe(options: { baseURL: string; timeoutMs?: number }) {
  const runner = new CliRunner(options.baseURL, options.timeoutMs);
  const shutdown = new AbortController();
  const server = Bun.serve({ hostname: "127.0.0.1", port: 0, development: false,
    maxRequestBodySize: 16 * 1024, idleTimeout: 35,
    async fetch(request, current) {
      try {
        enforceLocalAccess(request, "127.0.0.1", current.port!);
        if (shutdown.signal.aborted) throw new TurnError(503, "unavailable_error", "closed");
        const url = new URL(request.url);
        if (url.search) throw new TurnError(400, "invalid_request_error", "query rejected");
        if (request.method === "GET" && url.pathname === "/health") {
          return Response.json({ mode: "cli-probe", real_model_requests: 0, ...runner.stats() }, { headers });
        }
        if (request.method !== "POST" || url.pathname !== "/v1/messages") throw new TurnError(404, "not_found", "route rejected");
        if (request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== "application/json") {
          throw new TurnError(415, "invalid_request_error", "content type rejected");
        }
        if (["anthropic-beta", "anthropic-usage-limit", "authorization", "x-api-key", "cookie"].some((key) => request.headers.has(key))) {
          throw new TurnError(400, "invalid_request_error", "probe does not accept credentials or controls");
        }
        const signal = AbortSignal.any([request.signal, shutdown.signal]);
        const input = parseCliRequest(await readRequestBodyWithDeadline(request, 16 * 1024, signal, 3000));
        const run = runner.start(input, signal);
        if (!input.stream) {
          try {
            for await (const _ of run.chunks) { /* decoder collects a bounded single text message */ }
            return Response.json(run.result(), { headers });
          } finally { await run.cancel(); }
        }
        const body = new ReadableStream<Uint8Array>({
          async pull(controller) {
            try {
              const item = await run.chunks.next();
              if (item.done) controller.close(); else controller.enqueue(item.value);
            } catch {
              controller.error(new Error("cli_stream_failed"));
              await run.cancel();
            }
          },
          async cancel() { await run.cancel(); },
        }, { highWaterMark: 0 });
        return new Response(body, { headers: { ...headers, "content-type": "text/event-stream" } });
      } catch (error) { return errorResponse(error); }
    },
    error: () => errorResponse(undefined),
  });
  let closing: Promise<void> | undefined;
  return { url: `http://127.0.0.1:${server.port}`, stats: () => runner.stats(), diagnostics: () => runner.diagnostics(),
    stop: () => closing ??= (async () => {
      shutdown.abort();
      await stopWithDeadline(() => server.stop(true), () => runner.close(), 5000);
    })() };
}
