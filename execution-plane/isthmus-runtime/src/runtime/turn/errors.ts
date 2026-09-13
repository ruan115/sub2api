export class TurnError extends Error {
  constructor(
    readonly status: number,
    readonly type: string,
    message: string,
  ) {
    super(message);
    this.name = "TurnError";
  }
}

export class TurnCancelledError extends TurnError {
  constructor() {
    super(499, "request_cancelled", "Request cancelled");
    this.name = "TurnCancelledError";
  }
}

export function throwIfAborted(signal: AbortSignal): void {
  if (signal.aborted) throw new TurnCancelledError();
}
