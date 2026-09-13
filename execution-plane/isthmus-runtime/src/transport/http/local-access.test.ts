import { describe, expect, test } from "bun:test";
import { enforceLocalAccess } from "./local-access";

describe("local-demo Host and Origin policy", () => {
  test("allows literal bound address/port for CLI or exact same-origin HTTP", () => {
    for (const [hostname, authority] of [["127.0.0.1", "127.0.0.1:43210"], ["::1", "[::1]:43210"]] as const) {
      for (const origin of [undefined, `http://${authority}`]) {
        const headers: Record<string, string> = { host: authority };
        if (origin) headers.origin = origin;
        expect(() => enforceLocalAccess(new Request(`http://${authority}/v1/messages`, { headers }), hostname, 43210)).not.toThrow();
      }
    }
    expect(() => enforceLocalAccess(new Request("http://127.0.0.1/", { headers: { host: "127.0.0.1", origin: "http://127.0.0.1" } }), "127.0.0.1", 80)).not.toThrow();
  });

  test("rejects DNS rebind, ambiguous/foreign Host, wrong port and non-http URLs", () => {
    for (const host of ["rebind.invalid:43210", "localhost:43210", "127.1:43210", "127.0.0.1", "127.0.0.1:43211", "127.0.0.1:043210", "127.0.0.1:65536", "127.0.0.1:43210,evil.invalid", "[::1]:43210", "127.0.0.1:43210.", "user@127.0.0.1:43210"]) {
      expect(() => enforceLocalAccess(new Request("http://127.0.0.1:43210/", { headers: { host } }), "127.0.0.1", 43210)).toThrow();
    }
    for (const url of ["http://rebind.invalid:43210/", "https://127.0.0.1:43210/", "http://127.0.0.1:43211/"]) {
      expect(() => enforceLocalAccess(new Request(url, { headers: { host: "127.0.0.1:43210" } }), "127.0.0.1", 43210)).toThrow();
    }
  });

  test("rejects null, cross-origin, alternate scheme, multiple or malformed Origin", () => {
    for (const origin of ["null", "http://evil.invalid", "https://127.0.0.1:43210", "http://127.0.0.1:43211", "http://[::1]:43210", "http://127.0.0.1:43210/", "http://127.0.0.1:43210,http://evil.invalid", ""]) {
      expect(() => enforceLocalAccess(new Request("http://127.0.0.1:43210/", { headers: { origin } }), "127.0.0.1", 43210)).toThrow();
    }
  });
});
