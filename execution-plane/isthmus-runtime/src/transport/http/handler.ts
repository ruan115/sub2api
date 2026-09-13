import { TurnError, throwIfAborted } from "../../runtime/turn/errors";
import { parseTurnRequest, usageLimit } from "../../runtime/turn/request";
import type { TurnEngine } from "../../runtime/turn/types";
import { DEFAULT_BODY_LIMIT_BYTES, DEFAULT_BODY_READ_TIMEOUT_MS, DEFAULT_RESPONSE_LIMIT_BYTES, readRequestBodyWithDeadline, validateBodyLimit, validateBodyReadTimeout } from "../shared/body";
import { errorResponse, fakeHeaders } from "../shared/errors";
import { collectedResponse, streamingResponse } from "./response";

export interface HttpHandlerOptions {
  readonly bodyLimitBytes?: number;
  readonly responseLimitBytes?: number;
  readonly bodyReadTimeoutMs?: number;
}

export function createHttpHandler(engine: TurnEngine, options: HttpHandlerOptions = {}, shutdown?: AbortSignal) {
  if (engine.mode !== "fake") throw new Error("Only a fake TurnEngine is supported");
  const bodyLimit = validateBodyLimit(options.bodyLimitBytes ?? DEFAULT_BODY_LIMIT_BYTES);
  const responseLimit = validateBodyLimit(options.responseLimitBytes ?? DEFAULT_RESPONSE_LIMIT_BYTES);
  const bodyReadTimeout = validateBodyReadTimeout(options.bodyReadTimeoutMs ?? DEFAULT_BODY_READ_TIMEOUT_MS);
  return async (request: Request): Promise<Response> => {
    if (shutdown?.aborted) return errorResponse(new TurnError(503, "unavailable_error", "Fake runtime is closed"));
    const signal = shutdown ? AbortSignal.any([request.signal, shutdown]) : request.signal;
    const path = new URL(request.url).pathname;
    if (request.method === "GET" && (path === "/health" || path === "/")) {
      return Response.json({ ok: true, mode: "fake", service: "isthmus-fake" }, { headers: fakeHeaders() });
    }
    if (request.method !== "POST" || path !== "/v1/messages") {
      return errorResponse(new TurnError(404, "not_found", "no such route"));
    }
    try {
      // Deliberate local-demo policy: prevent no-cors text/plain form requests.
      if (request.headers.get("content-type")?.split(";", 1)[0].trim().toLowerCase() !== "application/json") {
        throw new TurnError(415, "invalid_request_error", "Fake messages endpoint requires application/json");
      }
      const bytes = await readRequestBodyWithDeadline(request, bodyLimit, signal, bodyReadTimeout);
      const input = parseTurnRequest(bytes, { usageLimit: usageLimit(request.headers.get("anthropic-usage-limit")) });
      throwIfAborted(signal);
      const turn = await engine.start(input, signal);
      if (signal.aborted) {
        await turn.cancel();
        throwIfAborted(signal);
      }
      if (input.stream && turn.start.status >= 200 && turn.start.status < 300) {
        return streamingResponse(turn, signal);
      }
      return await collectedResponse(turn, signal, responseLimit);
    } catch (error) {
      return errorResponse(error);
    }
  };
}
