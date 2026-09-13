import { FakeTurnEngine, type FakeTurnOptions } from "../runtime/turn/fake";
import { TurnError } from "../runtime/turn/errors";
import { createHttpHandler, type HttpHandlerOptions } from "../transport/http/handler";
import { WebSocketSession } from "../transport/websocket/session";
import type { WebSocketPeer } from "../transport/websocket/writer";

export interface FakeRuntimeOptions extends FakeTurnOptions, HttpHandlerOptions {
  readonly maxSessions?: number;
}

/** Pure assembly. There is intentionally no engine, executable or credential option. */
export function createFakeRuntime(options: FakeRuntimeOptions = {}) {
  const maxSessions = options.maxSessions ?? 16;
  if (!Number.isInteger(maxSessions) || maxSessions < 1 || maxSessions > 128) throw new Error("Invalid fake session capacity");
  const engine = new FakeTurnEngine(options);
  const shutdown = new AbortController();
  const sessions = new Set<WebSocketSession>();
  const handleHttp = createHttpHandler(engine, options, shutdown.signal);
  let closing: Promise<void> | undefined;
  return {
    mode: "fake" as const,
    handleHttp,
    stats: () => ({ ...engine.stats(), sessions: sessions.size }),
    createWebSocketSession(peer: WebSocketPeer): WebSocketSession {
      if (shutdown.signal.aborted) throw new TurnError(503, "unavailable_error", "Fake runtime is closed");
      if (sessions.size >= maxSessions) throw new TurnError(429, "rate_limit_error", "Fake session capacity exhausted");
      const session = new WebSocketSession(engine, {
        sendBinary: (frame) => peer.sendBinary(frame),
        close(code, reason) {
          sessions.delete(session);
          peer.close(code, reason);
        },
      }, options);
      sessions.add(session);
      return session;
    },
    close(): Promise<void> {
      if (closing) return closing;
      shutdown.abort();
      closing = Promise.all([...sessions].map((session) => session.close())).then(() => engine.close());
      return closing;
    },
  };
}
