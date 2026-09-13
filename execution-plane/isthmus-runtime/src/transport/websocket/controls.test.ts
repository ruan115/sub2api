import { describe, expect, test } from "bun:test";
import { CLIENT_FRAME_TAGS as TAG } from "../../protocol/websocket";
import { utf8 } from "../../../test/fixtures";
import { PendingControls } from "./controls";

describe("bounded pending WebSocket controls", () => {
  test("empty replace is present; append/remove empties are absent", () => {
    const controls = new PendingControls();
    controls.accept(TAG.BETA, new Uint8Array());
    controls.accept(TAG.BETA_REMOVE, new Uint8Array());
    controls.accept(TAG.BETA_REPLACE, new Uint8Array());
    expect(controls.take()).toEqual({ replaceBeta: [] });
    expect(controls.take()).toEqual({});
  });

  test("filters invalid beta tokens, keeps duplicate/order and last valid usage", () => {
    const controls = new PendingControls();
    controls.accept(TAG.BETA, utf8(" alpha,invalid/token, beta-1,alpha "));
    controls.accept(TAG.BETA_REMOVE, utf8("old.one"));
    controls.accept(TAG.BETA_REPLACE, utf8("replacement"));
    controls.accept(TAG.USAGE_LIMIT, utf8(" limit_1 "));
    controls.accept(TAG.USAGE_LIMIT, utf8("invalid/token"));
    expect(controls.take()).toEqual({ appendBeta: ["alpha", "beta-1", "alpha"], removeBeta: ["old.one"], replaceBeta: ["replacement"], usageLimit: "limit_1" });
  });

  test("byte and token overflow clear pending state", () => {
    const controls = new PendingControls();
    controls.accept(TAG.BETA, utf8("one"));
    expect(() => controls.accept(TAG.USAGE_LIMIT, new Uint8Array(8192))).toThrow();
    expect(controls.take()).toEqual({});
    controls.accept(TAG.BETA, utf8(Array(128).fill("a").join(",")));
    expect(() => controls.accept(TAG.BETA, utf8("b"))).toThrow();
    expect(controls.take()).toEqual({});
    controls.accept(TAG.BETA, utf8("a".repeat(8192)));
    expect(controls.take().appendBeta?.[0].length).toBe(8192);
  });
});
