import { describe, expect, test } from "bun:test";
import { TestPeer } from "../../../test/fixtures";
import { encodeFrame, SERVER_FRAME_TAGS as SERVER } from "../../protocol/websocket";
import { BoundedWriter } from "./writer";

const frame = () => encodeFrame("server-to-client", SERVER.END);

describe("bounded WebSocket writer", () => {
  test("backpressure waits for drain without resending bytes", async () => {
    const peer = new TestPeer(); peer.result = -1;
    const writer = new BoundedWriter(peer);
    let settled = false;
    const sending = writer.write(frame()).then(() => { settled = true; });
    await Promise.resolve();
    expect(settled).toBe(false);
    expect(writer.waitingForDrain).toBe(true);
    expect(peer.frames.length).toBe(1);
    writer.drain();
    await sending;
    expect(writer.waitingForDrain).toBe(false);
    expect(peer.frames.length).toBe(1);
    writer.close();
  });

  test("abort releases drain wait and subsequent writes can proceed", async () => {
    const peer = new TestPeer(); peer.result = -1;
    const writer = new BoundedWriter(peer);
    const controller = new AbortController();
    const sending = writer.write(frame(), controller.signal);
    controller.abort();
    await expect(sending).rejects.toMatchObject({ status: 499 });
    expect(writer.waitingForDrain).toBe(false);
    peer.result = 1;
    await writer.write(frame());
    expect(peer.frames.length).toBe(2);
    writer.close();
  });

  test("concurrent backpressured writes close instead of queueing", async () => {
    const peer = new TestPeer(); peer.result = -1;
    const writer = new BoundedWriter(peer);
    const first = writer.write(frame());
    const second = writer.write(frame());
    const outcomes = await Promise.allSettled([first, second]);
    expect(outcomes.map((outcome) => outcome.status)).toEqual(["rejected", "rejected"]);
    expect(peer.frames.length).toBe(1);
    expect(peer.closes).toEqual([{ code: 1013, reason: "fake transport backpressure limit" }]);
    expect(writer.waitingForDrain).toBe(false);
    writer.close();
    expect(peer.closes.length).toBe(1);
  });

  test("dropped/thrown sends close and do not leak exception details", async () => {
    for (const throws of [false, true]) {
      const closes: { code: number; reason: string }[] = [];
      const writer = new BoundedWriter({
        sendBinary() { if (throws) throw new Error("SYNTHETIC_PRIVATE"); return 0; },
        close(code, reason) { closes.push({ code, reason }); },
      });
      await expect(writer.write(frame())).rejects.toThrow("WebSocket send failed");
      expect(closes).toEqual([{ code: 1011, reason: "fake transport send failed" }]);
    }
  });
});
