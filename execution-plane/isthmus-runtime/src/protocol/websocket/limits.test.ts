import { expect, test } from "bun:test";
import {
  CLIENT_FRAME_TAGS,
  FrameCodecError,
  MAX_FRAME_BYTES,
  MAX_PAYLOAD_BYTES,
  decodeFrame,
  encodeFrame,
} from "./index";

test("the 64 MiB boundary includes the tag byte", () => {
  expect(MAX_FRAME_BYTES).toBe(67_108_864);
  expect(MAX_PAYLOAD_BYTES).toBe(67_108_863);

  const frame = new Uint8Array(MAX_FRAME_BYTES);
  frame[0] = CLIENT_FRAME_TAGS.REQUEST;
  frame[1] = 0xff;
  frame[frame.length - 1] = 0xfe;
  const decoded = decodeFrame("client-to-server", frame);
  expect(decoded.payload.byteLength).toBe(MAX_PAYLOAD_BYTES);
  expect(decoded.payload[0]).toBe(0xff);
  expect(decoded.payload[decoded.payload.length - 1]).toBe(0xfe);

  const encoded = encodeFrame("client-to-server", decoded.tag, decoded.payload);
  expect(encoded.byteLength).toBe(MAX_FRAME_BYTES);
  expect(encoded[0]).toBe(0x01);
  expect(encoded[1]).toBe(0xff);
  expect(encoded[encoded.length - 1]).toBe(0xfe);
});

test("one byte over the complete frame limit is rejected in both directions", () => {
  const oversized = new Uint8Array(MAX_FRAME_BYTES + 1);
  for (const [direction, tag] of [
    ["client-to-server", 0x01],
    ["server-to-client", 0x11],
  ] as const) {
    oversized[0] = tag;
    expect(() => decodeFrame(direction, oversized)).toThrow(FrameCodecError);
    expect(() => encodeFrame(direction, tag, oversized.subarray(1))).toThrow(
      "64 MiB including its tag byte",
    );
  }
});

test("a small view of a larger backing allocation is a small frame", () => {
  const backing = new Uint8Array(1024);
  backing[10] = CLIENT_FRAME_TAGS.REQUEST;
  backing[11] = 0xff;
  expect(decodeFrame("client-to-server", backing.subarray(10, 12)).payload).toEqual(
    Uint8Array.of(0xff),
  );
});
