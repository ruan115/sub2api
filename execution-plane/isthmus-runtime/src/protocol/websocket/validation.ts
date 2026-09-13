import { FrameCodecError } from "./errors";
import {
  CLIENT_FRAME_TAGS,
  SERVER_FRAME_TAGS,
  type FrameDirection,
  type FrameTag,
} from "./tags";

export const FRAME_TAG_BYTES = 1;
/** Maximum complete application message, including its one-byte tag. */
export const MAX_FRAME_BYTES = 64 * 1024 * 1024;
export const MAX_PAYLOAD_BYTES = MAX_FRAME_BYTES - FRAME_TAG_BYTES;

const clientTags = new Set<number>(Object.values(CLIENT_FRAME_TAGS));
const serverTags = new Set<number>(Object.values(SERVER_FRAME_TAGS));

export function validateDirection(direction: FrameDirection): void {
  if (direction !== "client-to-server" && direction !== "server-to-client") {
    throw new FrameCodecError("invalid_direction", "Unknown frame direction");
  }
}

export function validateFrameLength(byteLength: number): void {
  if (byteLength < FRAME_TAG_BYTES) {
    throw new FrameCodecError("empty_frame", "A frame must contain a tag byte");
  }
  if (byteLength > MAX_FRAME_BYTES) {
    throw new FrameCodecError(
      "frame_too_large",
      "A frame must not exceed 64 MiB including its tag byte",
    );
  }
}

export function validateTag(
  direction: FrameDirection,
  tag: number,
): asserts tag is FrameTag {
  validateDirection(direction);
  if (!clientTags.has(tag) && !serverTags.has(tag)) {
    throw new FrameCodecError("unknown_tag", "Unknown application frame tag");
  }
  const allowed = direction === "client-to-server" ? clientTags : serverTags;
  if (!allowed.has(tag)) {
    throw new FrameCodecError(
      "wrong_direction",
      "Application frame tag is not valid for this direction",
    );
  }
}
