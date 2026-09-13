import type { FrameDirection, FrameTag } from "./tags";
import {
  FRAME_TAG_BYTES,
  validateDirection,
  validateFrameLength,
  validateTag,
} from "./validation";

export interface DecodedFrame {
  readonly tag: FrameTag;
  /** Borrowed view of the input message; callers own its lifetime. */
  readonly payload: Uint8Array;
}

const EMPTY_PAYLOAD = new Uint8Array(0);

/** Encode one application message, copying its payload without interpretation. */
export function encodeFrame(
  direction: FrameDirection,
  tag: FrameTag,
  payload: Uint8Array = EMPTY_PAYLOAD,
): Uint8Array {
  validateTag(direction, tag);
  validateFrameLength(FRAME_TAG_BYTES + payload.byteLength);
  const frame = new Uint8Array(FRAME_TAG_BYTES + payload.byteLength);
  frame[0] = tag;
  frame.set(payload, FRAME_TAG_BYTES);
  return frame;
}

/** Decode a complete application message. Empty payloads remain valid bytes. */
export function decodeFrame(
  direction: FrameDirection,
  frame: Uint8Array,
): DecodedFrame {
  validateDirection(direction);
  validateFrameLength(frame.byteLength);
  const tag = frame[0];
  validateTag(direction, tag);
  return { tag, payload: frame.subarray(FRAME_TAG_BYTES) };
}
