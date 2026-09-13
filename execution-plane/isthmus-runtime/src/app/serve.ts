import { MAX_FRAME_BYTES } from "../protocol/websocket";
import { TurnError } from "../runtime/turn/errors";
import { errorResponse } from "../transport/shared/errors";
import { enforceLocalAccess } from "../transport/http/local-access";
import { DEFAULT_BODY_LIMIT_BYTES, validateBodyLimit } from "../transport/shared/body";
import { MAX_CONTROL_BYTES } from "../transport/websocket/controls";
import type { WebSocketSession } from "../transport/websocket/session";
import { createFakeRuntime, type FakeRuntimeOptions } from "./fake";
import { DEFAULT_SHUTDOWN_TIMEOUT_MS, stopWithDeadline, validateShutdownTimeout } from "./shutdown";

export interface FakeServerOptions extends FakeRuntimeOptions {
  readonly hostname?: "127.0.0.1" | "::1";
  readonly port?: number;
  readonly shutdownTimeoutMs?: number;
}
interface SocketData { session?: WebSocketSession }

/** Explicit opt-in listener factory. Importing this module never opens a port. */
export function serveFakeRuntime(options: FakeServerOptions = {}) {
  const hostname = options.hostname ?? "127.0.0.1";
  if (hostname !== "127.0.0.1" && hostname !== "::1") throw new Error("Fake listener must use a literal loopback address");
  const port = options.port ?? 0;
  if (!Number.isInteger(port) || port < 0 || port > 65535) throw new Error("Invalid fake listener port");
  const bodyLimit = validateBodyLimit(options.bodyLimitBytes ?? DEFAULT_BODY_LIMIT_BYTES);
  const shutdownTimeout = validateShutdownTimeout(options.shutdownTimeoutMs ?? DEFAULT_SHUTDOWN_TIMEOUT_MS);
  const messageLimit = Math.min(MAX_FRAME_BYTES, Math.max(bodyLimit, MAX_CONTROL_BYTES) + 1);
  const runtime = createFakeRuntime(options);
  let server: ReturnType<typeof Bun.serve<SocketData>>;
  try {
    server = Bun.serve<SocketData>({
      hostname, port, development: false, idleTimeout: 15,
      maxRequestBodySize: bodyLimit,
      async fetch(request, server) {
        try { enforceLocalAccess(request, hostname, server.port!); }
        catch (error) { return errorResponse(error); }
        if (new URL(request.url).pathname === "/v1/messages" && request.headers.get("upgrade")?.toLowerCase() === "websocket") {
          if (server.upgrade(request, { data: {} })) return undefined;
          return errorResponse(new TurnError(400, "invalid_request_error", "WebSocket upgrade failed"));
        }
        return runtime.handleHttp(request);
      },
      error() { return errorResponse(new TurnError(502, "api_error", "Fake listener failed")); },
      websocket: {
        // Deliberately no compression/keepalive parity claim in the fake slice.
        perMessageDeflate: false, idleTimeout: 30, maxPayloadLength: messageLimit,
        backpressureLimit: 128 * 1024, closeOnBackpressureLimit: true,
        open(socket) {
          try {
            socket.data.session = runtime.createWebSocketSession({
              sendBinary: (frame) => socket.sendBinary(frame),
              close: (code, reason) => {
                if (socket.readyState === WebSocket.OPEN) socket.close(code, reason);
              },
            });
          } catch { socket.close(1013, "fake session capacity unavailable"); }
        },
        message(socket, message) {
          socket.data.session?.receive(typeof message === "string" ? new TextEncoder().encode(message) : message);
        },
        drain(socket) { socket.data.session?.drain(); },
        close(socket) { void socket.data.session?.close(); },
      },
    });
  } catch (error) {
    void runtime.close();
    throw error;
  }
  let stopping: Promise<void> | undefined;
  let shutdownState: "running" | "stopping" | "stopped" | "failed" = "running";
  return {
    mode: "fake" as const,
    url: `http://${hostname === "::1" ? "[::1]" : hostname}:${server.port}`,
    stats: () => ({ ...runtime.stats(), shutdown: shutdownState }),
    stop(): Promise<void> {
      if (stopping) return stopping;
      // Native-first fixes client-close vs graceful-close ordering. Bun 1.3.9
      // also has a separately reproduced server-close bookkeeping hang: keep
      // wire close codes intact, bound that wait and report failure explicitly.
      shutdownState = "stopping";
      stopping = stopWithDeadline(() => server.stop(true), () => runtime.close(), shutdownTimeout)
        .then(() => { shutdownState = "stopped"; }, (error) => { shutdownState = "failed"; throw error; });
      return stopping;
    },
  };
}
