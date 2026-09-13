export interface TurnControls {
  readonly appendBeta?: readonly string[];
  readonly removeBeta?: readonly string[];
  /** Presence, including an empty list, is significant. */
  readonly replaceBeta?: readonly string[];
  readonly usageLimit?: string;
}

export interface TurnRequest {
  readonly body: Readonly<Record<string, unknown>>;
  readonly model: string;
  readonly stream: boolean;
  readonly controls: TurnControls;
}

export interface TurnStart {
  readonly status: number;
  readonly statusText: string;
  readonly headers: Readonly<Record<string, string>>;
}

export type TurnCompletion = "completed" | "cancelled" | "failed";

export interface TurnHandle {
  readonly start: TurnStart;
  readonly chunks: AsyncIterableIterator<Uint8Array>;
  readonly done: Promise<TurnCompletion>;
  cancel(): Promise<void>;
}

/** This slice's only implementation is fake; there is no real driver port yet. */
export interface TurnEngine {
  readonly mode: "fake";
  start(request: TurnRequest, signal: AbortSignal): Promise<TurnHandle>;
  close(): Promise<void>;
}
