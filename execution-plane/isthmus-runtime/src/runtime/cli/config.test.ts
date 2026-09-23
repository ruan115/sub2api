import { describe, expect, test } from "bun:test";
import { CLI_PATH, cliCommand, cliInput, cliPathForVersion, parseCliRequest, PROBE_MODEL, SYNTHETIC_TOKEN } from "./config";

const body = { model: PROBE_MODEL, max_tokens: 128, messages: [{ role: "user", content: "synthetic only" }], stream: true };
const encode = (value: unknown) => new TextEncoder().encode(JSON.stringify(value));
describe("CLI probe input and execution contract", () => {
  test("one prompt goes to stdin, never argv or shell", () => {
    const prompt = "synthetic $(touch /never-run) `id` --settings /untrusted";
    expect(parseCliRequest(encode({ ...body, messages: [{ role: "user", content: prompt }] }))).toEqual({ prompt, stream: true });
    const command = cliCommand("http://127.0.0.1:18765/capture-" + "a".repeat(24));
    expect(command.cmd.slice(0, 3)).toEqual(["/usr/bin/setsid", "--", CLI_PATH]);
    expect(command.cmd).not.toContain(prompt);
    expect(command.cmd).toContain("--include-partial-messages");
    expect(command.cmd[command.cmd.indexOf("--input-format") + 1]).toBe("stream-json");
    expect(command.cmd[command.cmd.indexOf("--output-format") + 1]).toBe("stream-json");
    expect(command.cmd[command.cmd.indexOf("--tools") + 1]).toBe("");
    expect(command.cmd[command.cmd.indexOf("--setting-sources") + 1]).toBe("");
    expect(command.env.CLAUDE_CODE_OAUTH_TOKEN).toBe(SYNTHETIC_TOKEN);
    expect(Object.keys(command.env).some((key) => /proxy|ssh|api_key/i.test(key))).toBe(false);
  });
  test("JSON-line stdin escapes newlines and cannot inject a second control message", () => {
    const prompt = 'synthetic\n{"type":"control_request"}\n你好';
    const encoded = new TextDecoder().decode(cliInput(prompt));
    expect(encoded.split("\n")).toHaveLength(2);
    expect(JSON.parse(encoded)).toEqual({ type: "user", message: { role: "user", content: prompt }, parent_tool_use_id: null });
  });
  test("rejects unsupported parameters, multi-turn, nontext and credentials", () => {
    for (const value of [null, [], { ...body, model: "other" }, { ...body, max_tokens: 129 },
      { ...body, stream: "true" }, { ...body, tools: [] }, { ...body, system: "not supported yet" },
      { ...body, messages: [...body.messages, ...body.messages] },
      { ...body, messages: [{ role: "assistant", content: "text" }] },
      { ...body, messages: [{ role: "user", content: "x".repeat(8193) }] },
      { ...body, messages: [{ role: "user", content: [{ type: "text", text: "no" }] }] }]) {
      expect(() => parseCliRequest(encode(value))).toThrow();
    }
    expect(() => parseCliRequest(new Uint8Array([255]))).toThrow();
  });
  test("differential runs a versioned sibling, and nothing else is spellable", () => {
    const url = "http://127.0.0.1:18765/capture-" + "c".repeat(24);
    expect(cliPathForVersion("2.1.280")).toBe("/opt/isthmus-probe/cli/2.1.280/claude");
    for (const version of ["2.1.258", "2.1.280"]) {
      const path = cliPathForVersion(version);
      expect(cliCommand(url, path).cmd.slice(0, 3)).toEqual(["/usr/bin/setsid", "--", path]);
    }
    // Same argv and environment regardless of version: the contract is what is under test.
    const { cmd: a, env: ea } = cliCommand(url, cliPathForVersion("2.1.258"));
    const { cmd: b, env: eb } = cliCommand(url, cliPathForVersion("2.1.280"));
    expect(a.slice(3)).toEqual(b.slice(3));
    expect(ea).toEqual(eb);
    for (const path of ["/opt/isthmus-probe/cli/2.1.280/../../../bin/sh", "/bin/sh",
      "/opt/isthmus-probe/cli//claude", "/opt/isthmus-probe/cli/2.1.280/claude ",
      "/opt/isthmus-probe/cli/latest/claude", "opt/isthmus-probe/bin/claude",
      "/opt/isthmus-probe/bin/claude\0/bin/sh", ""]) {
      expect(() => cliCommand(url, path)).toThrow();
    }
  });
  test("only literal loopback with a random synthetic path is allowed", () => {
    const suffix = "/capture-" + "b".repeat(24);
    for (const url of ["http://localhost:8000" + suffix, "http://127.1:8000" + suffix,
      "http://127.0.0.1:65536" + suffix, "http://127.0.0.1:8000/capture-short",
      "http://user@127.0.0.1:8000" + suffix, "http://127.0.0.1:8000" + suffix + "?x=1",
      "https://127.0.0.1:8000" + suffix, "http://216.106.185.119:8000" + suffix]) {
      expect(() => cliCommand(url)).toThrow();
    }
  });
});
