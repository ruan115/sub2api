import { TurnCancelledError, throwIfAborted } from "../../runtime/turn/errors";

/** Bun-compatible send result: >0 sent, -1 queued/backpressured, 0 dropped. */
export interface WebSocketPeer {
  sendBinary(frame: Uint8Array): number;
  close(code: number, reason: string): void;
}

/** One in-flight send plus one drain wait; no application-level send queue. */
export class BoundedWriter {
  private closed = false;
  private waiter?: { resolve(): void; reject(error: Error): void };

  constructor(private readonly peer: WebSocketPeer) {}

  async write(frame: Uint8Array, signal?: AbortSignal): Promise<void> {
    if (signal) throwIfAborted(signal);
    if (this.closed) throw new Error("WebSocket writer closed");
    if (this.waiter) {
      this.close(1013, "fake transport backpressure limit");
      throw new Error("Concurrent backpressured write");
    }
    let sent: number;
    try { sent = this.peer.sendBinary(frame); }
    catch {
      this.close(1011, "fake transport send failed");
      throw new Error("WebSocket send failed");
    }
    if (sent > 0) return;
    if (sent === 0) {
      this.close(1011, "fake transport send failed");
      throw new Error("WebSocket send failed");
    }
    await new Promise<void>((resolve, reject) => {
      const abort = () => finish(new TurnCancelledError());
      const finish = (error?: Error) => {
        if (this.waiter !== waiter) return;
        this.waiter = undefined;
        signal?.removeEventListener("abort", abort);
        error ? reject(error) : resolve();
      };
      const waiter = { resolve: () => finish(), reject: (error: Error) => finish(error) };
      this.waiter = waiter;
      signal?.addEventListener("abort", abort, { once: true });
      if (signal?.aborted) abort();
    });
  }

  drain(): void { this.waiter?.resolve(); }
  get waitingForDrain(): boolean { return this.waiter !== undefined; }
  get isClosed(): boolean { return this.closed; }

  close(code = 1000, reason = "fake session closed"): void {
    if (this.closed) return;
    this.closed = true;
    this.waiter?.reject(new Error("WebSocket writer closed"));
    try { this.peer.close(code, reason); } catch { /* teardown is best-effort */ }
  }
}
