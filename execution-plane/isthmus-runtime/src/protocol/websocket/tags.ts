/** Application tags inside a WebSocket message, not RFC 6455 opcodes. */
export const CLIENT_FRAME_TAGS = Object.freeze({
  REQUEST: 0x01,
  CANCEL: 0x02,
  BETA: 0x03,
  BETA_REMOVE: 0x04,
  BETA_REPLACE: 0x05,
  USAGE_LIMIT: 0x06,
} as const);

export const SERVER_FRAME_TAGS = Object.freeze({
  RESPONSE_START: 0x10,
  CHUNK: 0x11,
  END: 0x12,
  KEEPALIVE: 0x13,
  ERROR: 0x14,
} as const);

export type ClientFrameTag =
  (typeof CLIENT_FRAME_TAGS)[keyof typeof CLIENT_FRAME_TAGS];
export type ServerFrameTag =
  (typeof SERVER_FRAME_TAGS)[keyof typeof SERVER_FRAME_TAGS];
export type FrameTag = ClientFrameTag | ServerFrameTag;
export type FrameDirection = "client-to-server" | "server-to-client";
