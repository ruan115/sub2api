import { CLIENT_FRAME_TAGS as CLIENT, SERVER_FRAME_TAGS as SERVER, decodeFrame, encodeFrame } from "../../protocol/websocket";
import type { ServerFrameTag } from "../../protocol/websocket/tags";
import { TurnError, throwIfAborted } from "../../runtime/turn/errors";
import { parseTurnRequest } from "../../runtime/turn/request";
import type { TurnControls, TurnEngine, TurnHandle } from "../../runtime/turn/types";
import { collectResponseBody, DEFAULT_BODY_LIMIT_BYTES, DEFAULT_RESPONSE_LIMIT_BYTES, validateBodyLimit } from "../shared/body";
import { errorParts, fakeHeaders } from "../shared/errors";
import { PendingControls } from "./controls";
import { BoundedWriter, type WebSocketPeer } from "./writer";

export interface WebSocketSessionOptions {
  readonly bodyLimitBytes?: number;
  readonly responseLimitBytes?: number;
}
interface ActiveTurn { controller: AbortController; done: Promise<void>; handle?: TurnHandle }

/** Application protocol adapter only; the app layer owns socket creation. */
export class WebSocketSession {
  private readonly controls = new PendingControls();
  private readonly writer: BoundedWriter;
  private readonly bodyLimit: number;
  private readonly responseLimit: number;
  private active?: ActiveTurn;
  private closed = false;

  constructor(private readonly engine: TurnEngine, peer: WebSocketPeer, options: WebSocketSessionOptions = {}) {
    if (engine.mode !== "fake") throw new Error("Only a fake TurnEngine is supported");
    this.writer = new BoundedWriter(peer);
    this.bodyLimit = validateBodyLimit(options.bodyLimitBytes ?? DEFAULT_BODY_LIMIT_BYTES);
    this.responseLimit = validateBodyLimit(options.responseLimitBytes ?? DEFAULT_RESPONSE_LIMIT_BYTES);
  }

  /** Synchronous busy reservation prevents two same-tick REQUESTs starting. */
  receive(frame: Uint8Array): void {
    if (this.closed || this.writer.isClosed) return;
    try {
      const { tag, payload } = decodeFrame("client-to-server", frame);
      if (tag === CLIENT.CANCEL) {
        this.active?.controller.abort();
        return;
      }
      if (tag !== CLIENT.REQUEST) {
        this.controls.accept(tag, payload);
        return;
      }
      if (this.active) {
        this.controls.clear();
        this.sendError(new TurnError(409, "invalid_request_error", "a request is already in flight on this connection"));
        return;
      }
      const controls = this.controls.take();
      if (payload.length > this.bodyLimit) throw new TurnError(413, "request_too_large", "Request body exceeds the fake service limit");
      const active: ActiveTurn = { controller: new AbortController(), done: Promise.resolve() };
      this.active = active;
      // The codec borrows its payload. Copy before the caller can reuse a buffer.
      active.done = this.run(active, payload.slice(), controls);
    } catch (error) { this.sendError(error); }
  }

  private async send(tag: ServerFrameTag, payload?: Uint8Array, signal?: AbortSignal): Promise<void> {
    await this.writer.write(encodeFrame("server-to-client", tag, payload), signal);
  }

  private sendError(error: unknown): void {
    void this.send(SERVER.ERROR, new TextEncoder().encode(JSON.stringify(errorParts(error))))
      .catch(() => { void this.close(1011, "fake transport failed"); });
  }

  private async run(active: ActiveTurn, bytes: Uint8Array, controls: TurnControls): Promise<void> {
    const signal = active.controller.signal;
    let started = false;
    try {
      throwIfAborted(signal);
      const input = parseTurnRequest(bytes, controls);
      const turn = active.handle = await this.engine.start(input, signal);
      throwIfAborted(signal);
      const start = { ...turn.start, headers: Object.fromEntries(fakeHeaders(turn.start.headers).entries()) };
      if (start.status < 200 || start.status >= 300) {
        const body = await collectResponseBody(turn.chunks, this.responseLimit, signal);
        await this.send(SERVER.ERROR, new TextEncoder().encode(JSON.stringify({ ...start, body: new TextDecoder().decode(body) })), signal);
        return;
      }
      await this.send(SERVER.RESPONSE_START, new TextEncoder().encode(JSON.stringify(start)), signal);
      started = true;
      for await (const chunk of turn.chunks) {
        throwIfAborted(signal);
        await this.send(SERVER.CHUNK, chunk, signal);
      }
      throwIfAborted(signal);
      await this.send(SERVER.END, undefined, signal);
    } catch (error) {
      if (!signal.aborted && !this.closed && !this.writer.isClosed) {
        if (started) {
          this.closed = true;
          this.controls.clear();
          this.writer.close(1011, "fake stream failed");
        } else {
          try { await this.send(SERVER.ERROR, new TextEncoder().encode(JSON.stringify(errorParts(error))), signal); }
          catch { this.closed = true; this.writer.close(1011, "fake transport failed"); }
        }
      }
    } finally {
      await active.handle?.cancel();
      if (this.active === active) this.active = undefined;
    }
  }

  drain(): void { this.writer.drain(); }
  idle(): Promise<void> { return this.active?.done ?? Promise.resolve(); }
  get busy(): boolean { return this.active !== undefined; }
  get isClosed(): boolean { return this.closed || this.writer.isClosed; }
  get waitingForDrain(): boolean { return this.writer.waitingForDrain; }

  async close(code = 1000, reason = "fake session closed"): Promise<void> {
    this.closed = true;
    this.controls.clear();
    const active = this.active;
    active?.controller.abort();
    this.writer.close(code, reason);
    await active?.done;
  }
}
