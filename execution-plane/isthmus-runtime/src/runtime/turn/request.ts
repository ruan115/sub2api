import { TurnError } from "./errors";
import type { TurnControls, TurnRequest } from "./types";

export function usageLimit(value: string | null): string | undefined {
  const token = value?.trim();
  return token && token.length <= 64 && /^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(token) ? token : undefined;
}

export function parseTurnRequest(bytes: Uint8Array, controls: TurnControls = {}): TurnRequest {
  let body: unknown;
  try {
    body = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes));
  } catch {
    throw new TurnError(400, "invalid_request_error", "Request body must be UTF-8 JSON");
  }
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    throw new TurnError(400, "invalid_request_error", "Request body must be an object");
  }
  const record = body as Record<string, unknown>;
  if (!Array.isArray(record.messages) || record.messages.length === 0) {
    throw new TurnError(400, "invalid_request_error", "messages array is empty");
  }
  const model = typeof record.model === "string" && record.model.length > 0
    ? record.model
    : "fake-model";
  if (model.length > 128) {
    throw new TurnError(400, "invalid_request_error", "Fake model name exceeds 128 characters");
  }
  return { body: record, model, stream: record.stream === true, controls };
}
