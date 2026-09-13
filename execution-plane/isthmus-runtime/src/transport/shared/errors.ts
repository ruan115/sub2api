import { FrameCodecError } from "../../protocol/websocket";
import { TurnError } from "../../runtime/turn/errors";

const statusTexts: Record<number, string> = {
  400: "Bad Request", 403: "Forbidden", 404: "Not Found", 408: "Request Timeout", 409: "Conflict", 413: "Payload Too Large", 415: "Unsupported Media Type",
  429: "Too Many Requests", 499: "Client Closed Request", 502: "Bad Gateway",
  503: "Service Unavailable",
};

export function publicError(error: unknown): TurnError {
  if (error instanceof TurnError) return error;
  if (error instanceof FrameCodecError) {
    return new TurnError(error.code === "frame_too_large" ? 413 : 400,
      "invalid_request_error", "Malformed WebSocket application frame");
  }
  return new TurnError(502, "api_error", "Fake transport failed");
}

export function fakeHeaders(headers?: HeadersInit): Headers {
  const result = new Headers(headers);
  result.set("x-isthmus-runtime", "fake");
  result.set("cache-control", "no-store");
  return result;
}

export function errorParts(error: unknown) {
  const failure = publicError(error);
  return {
    status: failure.status,
    statusText: statusTexts[failure.status] ?? "Error",
    headers: Object.fromEntries(fakeHeaders({ "content-type": "application/json" }).entries()),
    body: JSON.stringify({ type: "error", error: { type: failure.type, message: failure.message } }),
  };
}

export function errorResponse(error: unknown): Response {
  const parts = errorParts(error);
  return new Response(parts.body, parts);
}
