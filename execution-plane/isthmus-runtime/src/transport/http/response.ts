import { TurnCancelledError, throwIfAborted } from "../../runtime/turn/errors";
import type { TurnHandle } from "../../runtime/turn/types";
import { collectResponseBody } from "../shared/body";
import { fakeHeaders } from "../shared/errors";

export function streamingResponse(turn: TurnHandle, signal: AbortSignal): Response {
  let finished = false;
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const cleanup = () => signal.removeEventListener("abort", abort);
  const abort = () => {
    if (finished) return;
    finished = true;
    cleanup();
    controller.error(new TurnCancelledError());
    void turn.cancel();
  };
  const body = new ReadableStream<Uint8Array>({
    start(value) { controller = value; },
    async pull(value) {
      if (finished) return;
      try {
        const item = await turn.chunks.next();
        if (finished) return;
        if (item.done) {
          finished = true;
          cleanup();
          value.close();
        } else {
          value.enqueue(item.value);
        }
      } catch (error) {
        if (finished) return;
        finished = true;
        cleanup();
        value.error(error);
        await turn.cancel();
      }
    },
    async cancel() {
      finished = true;
      cleanup();
      await turn.cancel();
    },
  }, { highWaterMark: 0 });
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted) abort();
  void turn.done.then((outcome) => { if (outcome === "cancelled") abort(); });
  return new Response(body, { ...turn.start, headers: fakeHeaders(turn.start.headers) });
}

export async function collectedResponse(
  turn: TurnHandle,
  signal: AbortSignal,
  limit: number,
): Promise<Response> {
  try {
    const bytes = await collectResponseBody(turn.chunks, limit, signal);
    throwIfAborted(signal);
    return new Response(bytes, { ...turn.start, headers: fakeHeaders(turn.start.headers) });
  } finally {
    await turn.cancel();
  }
}
