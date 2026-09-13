export type FrameCodecErrorCode =
  | "invalid_direction"
  | "unknown_tag"
  | "wrong_direction"
  | "empty_frame"
  | "frame_too_large";

/** Errors contain framing metadata only; payload bytes are never included. */
export class FrameCodecError extends Error {
  readonly code: FrameCodecErrorCode;

  constructor(code: FrameCodecErrorCode, message: string) {
    super(message);
    this.name = "FrameCodecError";
    this.code = code;
  }
}
