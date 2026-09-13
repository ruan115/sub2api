export const DEFAULT_SHUTDOWN_TIMEOUT_MS = 3000;

export class FakeShutdownTimeoutError extends Error {
  readonly code = "FAKE_SHUTDOWN_TIMEOUT";
  constructor(timeoutMs: number) {
    super(`Fake listener shutdown did not finish within ${timeoutMs} ms; shutdown is incomplete`);
    this.name = "FakeShutdownTimeoutError";
  }
}

export function validateShutdownTimeout(value: number): number {
  if (!Number.isInteger(value) || value < 1 || value > 60_000) throw new Error("Fake shutdown deadline must be between 1 and 60000 ms");
  return value;
}

/** Initiate native force-close first. A deadline is a FAILURE, never success. */
export async function stopWithDeadline(
  stopListener: () => void | Promise<void>,
  stopRuntime: () => void | Promise<void>,
  timeoutMs: number,
): Promise<void> {
  // Promise executors invoke both operations now, in order, catching sync throws.
  const listenerStopped = new Promise<void>((resolve) => resolve(stopListener()));
  const runtimeStopped = new Promise<void>((resolve) => resolve(stopRuntime()));
  const complete = Promise.allSettled([listenerStopped, runtimeStopped]).then((outcomes) => {
    for (const outcome of outcomes) {
      if (outcome.status === "rejected") throw outcome.reason;
    }
  });
  let timer!: ReturnType<typeof setTimeout>;
  const deadline = new Promise<never>((_, reject) => {
    timer = setTimeout(() => reject(new FakeShutdownTimeoutError(timeoutMs)), timeoutMs);
  });
  try { await Promise.race([complete, deadline]); }
  finally { clearTimeout(timer); }
}
