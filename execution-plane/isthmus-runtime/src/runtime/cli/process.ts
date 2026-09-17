import { cliCommand, cliInput, type CliRequest } from "./config";
import { CliEventDecoder, CliEventError, type CliEventDiagnostics } from "./events";
import { TurnError } from "../turn/errors";

export interface CliChild {
  pid: number;
  stdout: ReadableStream<Uint8Array>;
  stderr: ReadableStream<Uint8Array>;
  exited: Promise<number>;
  kill(signal: "SIGTERM" | "SIGKILL"): void;
}
export interface CliRun {
  chunks: AsyncIterableIterator<Uint8Array>;
  result(): Record<string, unknown>;
  cancel(): Promise<void>;
}
type Spawn = (config: ReturnType<typeof cliCommand>, prompt: string) => CliChild;
const spawn: Spawn = (config, prompt) => {
  if (process.platform !== "linux" || process.getuid?.() !== 1000) throw new Error("cli_linux_probe_user_required");
  return Bun.spawn(config.cmd, { cwd: config.cwd, env: config.env,
    stdin: cliInput(prompt), stdout: "pipe", stderr: "pipe" });
};
const failure = (status = 502) => new TurnError(status, "cli_execution_error",
  status === 504 ? "CLI execution deadline exceeded" : status === 499 ? "CLI request cancelled" : "CLI execution failed");

/** One request at a time; all tools disabled. Not a general process sandbox. */
export class CliRunner {
  private readonly config: ReturnType<typeof cliCommand>;
  private readonly active = new Set<CliRun>();
  private closed = false;
  private started = 0;
  private observation: { stage: string; exitCode?: number; stderrBytes?: number; eventError?: string;
    schema?: CliEventDiagnostics } = { stage: "idle" };
  constructor(baseURL: string, private readonly timeoutMs = 30_000,
    private readonly launch: Spawn = spawn,
    private readonly signalGroup: (pid: number, signal: "SIGTERM" | "SIGKILL") => void =
      (pid, signal) => { process.kill(-pid, signal); }) {
    this.config = cliCommand(baseURL);
    if (!Number.isInteger(timeoutMs) || timeoutMs < 100 || timeoutMs > 30_000) throw new Error("cli_deadline_rejected");
  }
  stats() { return { active: this.active.size, started: this.started, closed: this.closed }; }
  diagnostics() { return { ...this.observation }; }

  start(request: CliRequest, signal: AbortSignal): CliRun {
    if (this.closed) throw new TurnError(503, "unavailable_error", "CLI probe closed");
    if (signal.aborted) throw failure(499);
    if (this.active.size) throw new TurnError(429, "rate_limit_error", "CLI probe busy");
    let child: CliChild;
    this.observation = { stage: "spawn" };
    try { child = this.launch(this.config, request.prompt); } catch { throw failure(); }
    this.observation.stage = "running";
    const observation = this.observation;
    this.started++;
    const reader = child.stdout.getReader();
    const errors = child.stderr.getReader();
    const decoder = new CliEventDecoder();
    let issue: TurnError | undefined;
    let reaped = false;
    let stopping: Promise<void> | undefined;
    let timer: ReturnType<typeof setTimeout>;
    const send = (kind: "SIGTERM" | "SIGKILL") => {
      try { this.signalGroup(child.pid, kind); } catch (error: any) {
        if (error?.code !== "ESRCH") throw failure();
      }
      // Also cover cancellation before setsid has established the new group.
      if (!reaped) child.kill(kind);
    };
    const exited = child.exited.then((code) => { reaped = true; observation.exitCode = code; return code; });
    const stop = () => stopping ??= (async () => {
      // This tool-disabled probe has no graceful tool shutdown contract. Kill
      // the owned live group once, before awaiting/reaping its leader. Never
      // signal a remembered numeric PGID after exit: it may have been reused.
      if (!reaped) send("SIGKILL");
      await exited;
    })();
    const abort = (reason: TurnError) => {
      issue ??= reason;
      void stop().catch(() => { issue ??= failure(); });
      void reader.cancel().catch(() => {});
      // A reaped leader does not imply EOF: an inherited pipe may still be
      // open. Cancellation must wake both readers before awaiting cleanup.
      void errors.cancel().catch(() => {});
      // Reap independently of a stalled downstream consumer resuming its pull.
      void finish().catch(() => { issue ??= failure(); });
    };
    const onAbort = () => abort(failure(499));
    signal.addEventListener("abort", onAbort, { once: true });
    timer = setTimeout(() => abort(failure(504)), this.timeoutMs);
    const stderr = (async () => {
      let total = 0;
      try {
        while (true) {
          const item = await errors.read();
          if (item.done) break;
          total += item.value.length;
          observation.stderrBytes = total;
          if (total > 64 * 1024) { abort(failure()); break; }
        }
      } catch { abort(failure()); }
      finally { await errors.cancel().catch(() => {}); errors.releaseLock(); }
    })();
    let cleanup: Promise<void> | undefined;
    const finish = () => cleanup ??= (async () => {
      try { await stop(); await stderr; }
      finally {
        clearTimeout(timer);
        signal.removeEventListener("abort", onAbort);
        await reader.cancel().catch(() => {});
        if (reaped) this.active.delete(handle);
      }
    })();
    async function* generate(): AsyncGenerator<Uint8Array> {
      try {
        while (true) {
          const item = await reader.read();
          if (issue) throw issue;
          if (item.done) break;
          for (const bytes of decoder.push(item.value)) {
            if (issue) throw issue;
            yield bytes;
          }
        }
        const code = await exited;
        await stderr;
        if (issue) throw issue;
        const tail = decoder.finish(code);
        await finish();
        for (const bytes of tail) {
          if (issue) throw issue;
          observation.stage = "completed";
          yield bytes;
        }
      } catch (error) {
        observation.stage = "failed";
        if (error instanceof CliEventError) {
          observation.eventError = error.code;
          observation.schema = decoder.diagnostics();
        }
        throw error instanceof TurnError ? error : failure();
      }
      finally { await finish(); }
    }
    const handle: CliRun = {
      chunks: generate(),
      result: () => decoder.message(),
      cancel: async () => {
        abort(failure(499));
        await finish();
        await handle.chunks.return?.(undefined);
      },
    };
    this.active.add(handle);
    if (signal.aborted) onAbort();
    return handle;
  }
  async close() { this.closed = true; await Promise.all([...this.active].map((run) => run.cancel())); }
}
