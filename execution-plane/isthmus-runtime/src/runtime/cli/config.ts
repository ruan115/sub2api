import { TurnError } from "../turn/errors";

export const CLI_PATH = "/opt/isthmus-probe/bin/claude";
/** Versioned siblings of CLI_PATH, staged one per release for contract differential. */
export const cliPathForVersion = (version: string) => `/opt/isthmus-probe/cli/${version}/claude`;
export const PROBE_MODEL = "claude-sonnet-5";
export const SYNTHETIC_TOKEN = "synthetic-probe-token-not-a-real-credential";
export interface CliRequest { prompt: string; stream: boolean }

/** One SDK user record and EOF; the session-pool/control channel is a later slice. */
export function cliInput(prompt: string): Uint8Array {
  return new TextEncoder().encode(JSON.stringify({ type: "user", message: { role: "user", content: prompt },
    parent_tool_use_id: null }) + "\n");
}

/** Deliberately narrow first slice; unsupported fields never disappear silently. */
export function parseCliRequest(bytes: Uint8Array): CliRequest {
  const invalid = () => new TurnError(400, "invalid_request_error", "CLI probe accepts one text message, fixed model and max_tokens=128 only");
  let value: any;
  try { value = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(bytes)); } catch { throw invalid(); }
  if (!value || typeof value !== "object" || Array.isArray(value)
      || Object.keys(value).some((key) => !["model", "messages", "max_tokens", "stream"].includes(key))
      || value.model !== PROBE_MODEL || value.max_tokens !== 128
      || (value.stream !== undefined && typeof value.stream !== "boolean")
      || !Array.isArray(value.messages) || value.messages.length !== 1) throw invalid();
  const message = value.messages[0];
  if (!message || Object.keys(message).sort().join(",") !== "content,role" || message.role !== "user"
      || typeof message.content !== "string" || !message.content.trim()
      || new TextEncoder().encode(message.content).length > 8192 || message.content.includes("\0")) throw invalid();
  return { prompt: message.content, stream: value.stream === true };
}

export function cliCommand(baseURL: string, cliPath: string = CLI_PATH) {
  // Literal spelling is checked before URL normalization (127.1, encoded paths,
  // queries and URL credentials must not become a permitted endpoint).
  if (!/^http:\/\/127\.0\.0\.1:[1-9][0-9]{0,4}\/capture-[a-f0-9]{24,64}$/.test(baseURL)
      || Number(new URL(baseURL).port) > 65535) throw new Error("cli_probe_loopback_required");
  // The executable is staged, never caller-supplied: only the default probe path
  // or a versioned sibling. Relative segments and other roots stay unspellable.
  if (!/^\/opt\/isthmus-probe\/(bin|cli\/[0-9]+\.[0-9]+\.[0-9]+)\/claude$/.test(cliPath))
    throw new Error("cli_probe_executable_rejected");
  return {
    cmd: ["/usr/bin/setsid", "--", cliPath, "--print", "--safe-mode", "--tools", "",
      "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--setting-sources", "",
      "--no-session-persistence", "--max-turns", "1", "--thinking", "disabled",
      "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
      "--include-partial-messages", "--no-chrome", "--model", PROBE_MODEL],
    cwd: "/home/claude",
    // Never spread process.env. These are synthetic-only test credentials.
    env: { PATH: "/usr/bin:/bin", HOME: "/home/claude", USER: "claude", LOGNAME: "claude",
      LANG: "C.UTF-8", LC_ALL: "C.UTF-8", CLAUDE_CONFIG_DIR: "/home/claude/config",
      CLAUDE_SECURESTORAGE_CONFIG_DIR: "/home/claude/secure",
      CLAUDE_CODE_OAUTH_TOKEN: SYNTHETIC_TOKEN, ANTHROPIC_BASE_URL: baseURL,
      CLAUDE_CODE_OAUTH_401_WAIT_MS: "0", CLAUDE_CODE_MAX_RETRIES: "0",
      CLAUDE_CODE_MAX_OUTPUT_TOKENS: "128", CLAUDE_CODE_DISABLE_NONSTREAMING_FALLBACK: "1",
      CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1", CLAUDE_CODE_SKIP_PROMPT_HISTORY: "1",
      CLAUDE_CODE_DISABLE_AUTO_MEMORY: "1", CLAUDE_CODE_DISABLE_BACKGROUND_TASKS: "1",
      CLAUDE_CODE_DISABLE_CRON: "1", DISABLE_AUTO_COMPACT: "1", DISABLE_AUTOUPDATER: "1",
      DISABLE_TELEMETRY: "1", DISABLE_ERROR_REPORTING: "1",
      ENABLE_CLAUDEAI_MCP_SERVERS: "false", ENABLE_TOOL_SEARCH: "false" },
  };
}
