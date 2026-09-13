import { throwIfAborted, TurnError } from "../../runtime/turn/errors";

export const DEFAULT_BODY_LIMIT_BYTES = 1024 * 1024;
export const DEFAULT_RESPONSE_LIMIT_BYTES = 1024 * 1024;
export const DEFAULT_BODY_READ_TIMEOUT_MS = 10_000;

export function validateBodyReadTimeout(timeout: number): number {
  if (!Number.isInteger(timeout) || timeout < 1 || timeout > 60_000) throw new Error("Fake body deadline must be between 1 and 60000 ms");
  return timeout;
}

/** Deadline applies only to inbound body reads; timers are removed on all exits. */
export async function readRequestBodyWithDeadline(request: Request, limit: number, signal: AbortSignal, timeoutMs: number): Promise<Uint8Array> {
  const deadline = new AbortController();
  const timer = setTimeout(() => deadline.abort(), timeoutMs);
  try {
    return await readRequestBody(request, limit, AbortSignal.any([signal, deadline.signal]));
  } catch (error) {
    if (deadline.signal.aborted && !signal.aborted) throw new TurnError(408, "request_timeout", "Fake request body read timed out");
    throw error;
  } finally { clearTimeout(timer); }
}

export function validateBodyLimit(limit: number): number {
  if (!Number.isInteger(limit) || limit < 1 || limit > 16 * 1024 * 1024) {
    throw new Error("Fake byte limit must be between 1 byte and 16 MiB");
  }
  return limit;
}

function join(chunks: Uint8Array[], total: number): Uint8Array {
  const bytes = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    bytes.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return bytes;
}

/** Input must be buffered for JSON parsing, but is counted before retention. */
export async function readRequestBody(request: Request, limit: number, signal: AbortSignal = request.signal): Promise<Uint8Array> {
  throwIfAborted(signal);
  const length = request.headers.get("content-length");
  if (length && /^\d+$/.test(length) && Number(length) > limit) {
    void request.body?.cancel().catch(() => {});
    throw new TurnError(413, "request_too_large", "Request body exceeds the fake service limit");
  }
  if (!request.body) return new Uint8Array();
  const reader = request.body.getReader();
  const abort = () => { void reader.cancel().catch(() => {}); };
  signal.addEventListener("abort", abort, { once: true });
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    if (signal.aborted) abort();
    while (true) {
      const item = await reader.read();
      throwIfAborted(signal);
      if (item.done) break;
      total += item.value.byteLength;
      if (total > limit) {
        void reader.cancel().catch(() => {});
        throw new TurnError(413, "request_too_large", "Request body exceeds the fake service limit");
      }
      chunks.push(item.value.slice());
    }
    return join(chunks, total);
  } finally {
    signal.removeEventListener("abort", abort);
    reader.releaseLock();
  }
}

/** Only non-streaming/error paths collect output. Streaming adapters never call this. */
export async function collectResponseBody(
  iterator: AsyncIterableIterator<Uint8Array>,
  limit: number,
  signal: AbortSignal,
): Promise<Uint8Array> {
  const chunks: Uint8Array[] = [];
  let total = 0;
  for await (const bytes of iterator) {
    throwIfAborted(signal);
    total += bytes.byteLength;
    if (total > limit) {
      throw new TurnError(502, "api_error", "Fake response exceeds the collection limit");
    }
    chunks.push(bytes.slice());
  }
  throwIfAborted(signal);
  return join(chunks, total);
}
