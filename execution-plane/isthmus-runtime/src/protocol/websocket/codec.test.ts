import { describe, expect, test } from "bun:test";
import {
  CLIENT_FRAME_TAGS,
  SERVER_FRAME_TAGS,
  FrameCodecError,
  decodeFrame,
  encodeFrame,
  type FrameCodecErrorCode,
  type FrameDirection,
  type FrameTag,
} from "./index";

// Numeric fixtures are independent of the constants under test and reflect the
// recovered Pt table. Payloads below are synthetic, never captured traffic.
const fixtures = [
  { name: "REQUEST", direction: "client-to-server", tag: 0x01 },
  { name: "CANCEL", direction: "client-to-server", tag: 0x02 },
  { name: "BETA", direction: "client-to-server", tag: 0x03 },
  { name: "BETA_REMOVE", direction: "client-to-server", tag: 0x04 },
  { name: "BETA_REPLACE", direction: "client-to-server", tag: 0x05 },
  { name: "USAGE_LIMIT", direction: "client-to-server", tag: 0x06 },
  { name: "RESPONSE_START", direction: "server-to-client", tag: 0x10 },
  { name: "CHUNK", direction: "server-to-client", tag: 0x11 },
  { name: "END", direction: "server-to-client", tag: 0x12 },
  { name: "KEEPALIVE", direction: "server-to-client", tag: 0x13 },
  { name: "ERROR", direction: "server-to-client", tag: 0x14 },
] as const;

function expectCodecError(run: () => unknown, code: FrameCodecErrorCode): void {
  let thrown: unknown;
  try {
    run();
  } catch (error) {
    thrown = error;
  }
  expect(thrown).toBeInstanceOf(FrameCodecError);
  expect((thrown as FrameCodecError).code).toBe(code);
}

describe("known wire tags", () => {
  for (const fixture of fixtures) {
    test(`${fixture.name} preserves tag and arbitrary payload bytes`, () => {
      const tags = { ...CLIENT_FRAME_TAGS, ...SERVER_FRAME_TAGS };
      expect(tags[fixture.name]).toBe(fixture.tag);
      const payload = Uint8Array.of(0x00, 0xff, 0xc0, 0xaf, 0x0a, 0x80);
      const frame = encodeFrame(fixture.direction, fixture.tag, payload);
      expect(frame).toEqual(Uint8Array.of(fixture.tag, ...payload));
      const decoded = decodeFrame(fixture.direction, frame);
      expect(decoded.tag).toBe(fixture.tag);
      expect(decoded.payload).toEqual(payload);
    });

    test(`${fixture.name} accepts a tag-only frame without interpreting it`, () => {
      const frame = Uint8Array.of(fixture.tag);
      expect(encodeFrame(fixture.direction, fixture.tag)).toEqual(frame);
      expect(
        encodeFrame(fixture.direction, fixture.tag, new Uint8Array(0)),
      ).toEqual(frame);
      expect(decodeFrame(fixture.direction, frame).payload.byteLength).toBe(0);
    });

    test(`${fixture.name} is rejected in the opposite direction`, () => {
      const wrongDirection =
        fixture.direction === "client-to-server"
          ? "server-to-client"
          : "client-to-server";
      expectCodecError(
        () => encodeFrame(wrongDirection, fixture.tag),
        "wrong_direction",
      );
      expectCodecError(
        () => decodeFrame(wrongDirection, Uint8Array.of(fixture.tag)),
        "wrong_direction",
      );
    });
  }
});

describe("invalid framing", () => {
  for (const direction of ["client-to-server", "server-to-client"] as const) {
    test(`${direction} rejects an empty message, not an empty payload`, () => {
      expectCodecError(
        () => decodeFrame(direction, new Uint8Array(0)),
        "empty_frame",
      );
    });

    for (const tag of [0x00, 0x07, 0x0f, 0x15, 0xff]) {
      test(`${direction} rejects unknown tag ${tag}`, () => {
        expectCodecError(
          () => decodeFrame(direction, Uint8Array.of(tag)),
          "unknown_tag",
        );
        expectCodecError(
          () => encodeFrame(direction, tag as FrameTag),
          "unknown_tag",
        );
      });
    }
  }

  for (const tag of [-1, 256, 1.5, Number.NaN]) {
    test(`encoding rejects tag ${String(tag)} before byte coercion`, () => {
      expectCodecError(
        () => encodeFrame("client-to-server", tag as FrameTag),
        "unknown_tag",
      );
    });
  }

  test("invalid direction is rejected by both operations", () => {
    const direction = "sideways" as FrameDirection;
    expectCodecError(() => encodeFrame(direction, 0x01), "invalid_direction");
    expectCodecError(
      () => decodeFrame(direction, Uint8Array.of(0x01)),
      "invalid_direction",
    );
  });
});

describe("buffer ownership and byte preservation", () => {
  test("encoding copies only the payload view, including every byte value", () => {
    const backing = new Uint8Array(260).fill(0x55);
    const payload = backing.subarray(2, 258);
    for (let i = 0; i < payload.length; i++) payload[i] = i;
    const frame = encodeFrame("server-to-client", SERVER_FRAME_TAGS.CHUNK, payload);
    expect(frame.byteLength).toBe(257);
    expect(frame[0]).toBe(0x11);
    expect(frame.subarray(1)).toEqual(payload);
    payload.fill(0);
    expect(frame[256]).toBe(0xff);
  });

  test("decoding borrows only the supplied frame view", () => {
    const backing = Uint8Array.of(0x55, 0x11, 0x00, 0xff, 0x66);
    const frame = backing.subarray(1, 4);
    const decoded = decodeFrame("server-to-client", frame);
    expect(decoded.payload).toEqual(Uint8Array.of(0x00, 0xff));
    expect(decoded.payload.buffer).toBe(frame.buffer);
    expect(decoded.payload.byteOffset).toBe(frame.byteOffset + 1);
    frame[1] = 0x7f;
    expect(decoded.payload[0]).toBe(0x7f);
  });

  test("control payload bytes are preserved rather than rejected or parsed", () => {
    const payload = new TextEncoder().encode("synthetic,not-a-header\r\n");
    const frame = encodeFrame("client-to-server", CLIENT_FRAME_TAGS.CANCEL, payload);
    expect(decodeFrame("client-to-server", frame).payload).toEqual(payload);
  });
});
