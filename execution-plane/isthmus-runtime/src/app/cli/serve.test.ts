import { expect, spyOn, test } from "bun:test";
import { serveCliProbe } from "./serve";
import { CliRunner } from "../../runtime/cli/process";

const baseURL = "http://127.0.0.1:12345/capture-" + "a".repeat(24);
const input = { model: "claude-sonnet-5", max_tokens: 128, messages: [{ role: "user", content: "synthetic" }] };
async function fixture(check: (app: ReturnType<typeof serveCliProbe>, config: any, stops: () => number) => Promise<void>) {
  let config: any, stopped = 0;
  const listener = spyOn(Bun, "serve").mockImplementation(((options: any) => {
    config = options; return { port: 43210, stop() { stopped++; } };
  }) as never);
  try {
    const app = serveCliProbe({ baseURL });
    try { await check(app, config, () => stopped); } finally { await app.stop(); }
  } finally { listener.mockRestore(); }
}
const request = (headers: Record<string, string> = {}, body: unknown = input) => new Request(
  "http://127.0.0.1:43210/v1/messages", { method: "POST", headers: { "content-type": "application/json", ...headers }, body: JSON.stringify(body) });

test("CLI listener is explicit loopback-only and health makes the probe boundary visible", async () => {
  await fixture(async (app, config) => {
    expect(config).toMatchObject({ hostname: "127.0.0.1", port: 0, maxRequestBodySize: 16384 });
    const health = await config.fetch(new Request(app.url + "/health"), { port: 43210 });
    expect((await health.json()).mode).toBe("cli-probe");
    expect(health.headers.get("x-isthmus-usage")).toBe("synthetic");
    expect(app.stats().started).toBe(0);
  });
});
test("bad origins, credentials, controls and unsupported request fields never spawn", async () => {
  const start = spyOn(CliRunner.prototype, "start").mockImplementation(() => { throw new Error("must not spawn"); });
  try {
    await fixture(async (_, config) => {
      for (const headers of [{ origin: "https://evil.invalid" }, { host: "other.invalid" },
        { authorization: "synthetic-rejected" }, { "x-api-key": "synthetic-rejected" }, { "anthropic-beta": "x" }]) {
        expect((await config.fetch(request(headers), { port: 43210 })).status).toBeGreaterThanOrEqual(400);
      }
      expect((await config.fetch(request({}, { ...input, tools: [] }), { port: 43210 })).status).toBe(400);
      expect((await config.fetch(request({ "content-type": "text/plain" }), { port: 43210 })).status).toBe(415);
      expect(start).not.toHaveBeenCalled();
    });
  } finally { start.mockRestore(); }
});
test("unexpected process errors are payload-free, not reflected into HTTP", async () => {
  const start = spyOn(CliRunner.prototype, "start").mockImplementation(() => { throw new Error("PRIVATE_NOT_FOR_RESPONSE"); });
  try {
    await fixture(async (_, config) => {
      const response = await config.fetch(request(), { port: 43210 });
      expect(response.status).toBe(502);
      expect(await response.text()).not.toContain("PRIVATE_NOT_FOR_RESPONSE");
    });
  } finally { start.mockRestore(); }
});
test("listener is still stopped when process cleanup rejects", async () => {
  let stopped = 0;
  const listener = spyOn(Bun, "serve").mockImplementation((() => ({ port: 43210, stop() { stopped++; } })) as never);
  const close = spyOn(CliRunner.prototype, "close").mockRejectedValue(new Error("synthetic_cleanup_failed"));
  try {
    const app = serveCliProbe({ baseURL });
    await expect(app.stop()).rejects.toThrow();
    expect(stopped).toBe(1);
  } finally { close.mockRestore(); listener.mockRestore(); }
});
