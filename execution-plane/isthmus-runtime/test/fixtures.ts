import { decodeFrame, encodeFrame, CLIENT_FRAME_TAGS, type ServerFrameTag } from "../src/protocol/websocket";
import type { WebSocketPeer } from "../src/transport/websocket/writer";

export const utf8 = (text: string) => new TextEncoder().encode(text);
export const requestBytes = (stream = true) => utf8(JSON.stringify({ model: "synthetic-model", stream, messages: [{ role: "user", content: "SYNTHETIC_INPUT_DO_NOT_ECHO" }] }));
export const requestFrame = (stream = true) => encodeFrame("client-to-server", CLIENT_FRAME_TAGS.REQUEST, requestBytes(stream));
export function joinBytes(chunks: readonly Uint8Array[]): Uint8Array {
  const joined = new Uint8Array(chunks.reduce((total, bytes) => total + bytes.length, 0));
  let offset = 0;
  for (const chunk of chunks) { joined.set(chunk, offset); offset += chunk.length; }
  return joined;
}
export async function collect(iterator: AsyncIterable<Uint8Array>): Promise<Uint8Array> {
  const result: Uint8Array[] = [];
  for await (const chunk of iterator) result.push(chunk);
  return joinBytes(result);
}

/** Finite synthetic test recordings only, not a production send queue. */
export class TestPeer implements WebSocketPeer {
  readonly frames: { tag: ServerFrameTag; payload: Uint8Array }[] = [];
  readonly closes: { code: number; reason: string }[] = [];
  result = 1;
  private waiters = new Set<() => void>();
  sendBinary(frame: Uint8Array): number {
    const decoded = decodeFrame("server-to-client", frame);
    this.frames.push({ tag: decoded.tag as ServerFrameTag, payload: decoded.payload.slice() });
    for (const waiter of [...this.waiters]) waiter();
    return this.result;
  }
  close(code: number, reason: string): void { this.closes.push({ code, reason }); }
  until(predicate: () => boolean): Promise<void> {
    if (predicate()) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const timeout = setTimeout(() => { this.waiters.delete(check); reject(new Error("Synthetic frame wait timed out")); }, 2000);
      const check = () => {
        if (!predicate()) return;
        clearTimeout(timeout);
        this.waiters.delete(check);
        resolve();
      };
      this.waiters.add(check);
    });
  }
}
