import { describe, expect, test } from "bun:test";
import { stopWithDeadline, validateShutdownTimeout } from "./shutdown";

describe("honest bounded native/runtime shutdown", () => {
  test("initiates native force-close before runtime cleanup and waits for both", async () => {
    const events: string[] = [];
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let settled = false;
    const stopping = stopWithDeadline(() => { events.push("native"); return held; }, () => { events.push("runtime"); }, 1000);
    const outcome = stopping.then(() => { settled = true; });
    expect(events).toEqual(["native", "runtime"]);
    await Promise.resolve();
    expect(settled).toBe(false);
    release();
    await outcome;
    expect(settled).toBe(true);
  });

  test("never-settling native stop rejects with explicit incomplete-shutdown deadline", async () => {
    let cleaned = false;
    const stopping = stopWithDeadline(() => new Promise<void>(() => {}), () => { cleaned = true; }, 5);
    await expect(stopping).rejects.toMatchObject({ name: "FakeShutdownTimeoutError", code: "FAKE_SHUTDOWN_TIMEOUT" });
    expect(cleaned).toBe(true);
  });

  test("synchronous failure still initiates and awaits the other cleanup side", async () => {
    for (const failureSide of ["native", "runtime"]) {
      let release!: () => void;
      const held = new Promise<void>((resolve) => { release = resolve; });
      const events: string[] = [];
      const operation = (name: string) => () => { events.push(name); if (name === failureSide) throw new Error("Synthetic shutdown failure"); return held; };
      let settled = false;
      const result = stopWithDeadline(operation("native"), operation("runtime"), 1000).then(() => "ok", (error: Error) => { settled = true; return error.message; });
      expect(events).toEqual(["native", "runtime"]);
      await Promise.resolve();
      expect(settled).toBe(false);
      release();
      expect(await result).toBe("Synthetic shutdown failure");
    }
  });

  test("deadline configuration is finite and explicit", () => {
    for (const value of [0, -1, 60001, Number.NaN, 1.5]) expect(() => validateShutdownTimeout(value)).toThrow();
    expect(validateShutdownTimeout(3000)).toBe(3000);
  });
});
