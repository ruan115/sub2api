# Single-instance CLI execution slice

This is a real CLI subprocess adapter, not the fake turn engine and not a
production launcher. `app/cli/serve.ts` explicitly creates an ephemeral loopback
HTTP listener; importing any module starts nothing. Sub2 pricing/auth and the
CCMAX gateway are unchanged.

- `config.ts`: one text user message, fixed model, max_tokens128; explicit
  command/environment and only a literal loopback synthetic upstream URL.
- `process.ts`: one active child; stdin (never a shell/argv prompt), finite
  deadline, process-group termination/reap, bounded stderr discard and lazy
  stdout consumption. Requires the separately verified Linux container.
- `events.ts`: bounded stream-json decoder, text-only Anthropic JSON/SSE;
  `message_stop` is withheld until a complete stream, CLI success result,
  exit0, and process cleanup. Unknown protocol behavior fails closed.

The CLI executable is the separately hashed official2.1.258; candidate Bun1.4.2
runs this adapter only in the lab. Project/CI Bun1.3.9 remains unchanged.
All credentials are synthetic. User/project settings, MCP, tools and session
persistence are disabled. Raw stdout/stderr/headers/prompts are not logged;
diagnostics contain fixed schema categories and numeric process counts only.
CLI `system/status` is an observed non-content record, not a success event.

## Scope and parity

Supported request: POST `/v1/messages`, application/json, one text user message,
model `claude-sonnet-5`, max_tokens128, optional boolean stream. Unsupported
parameters, auth headers and controls fail before spawn, not silently ignored.
Responses explicitly mark `x-isthmus-runtime: cli-probe` and synthetic usage.
Nonstream errors return a fixed502/504/499; failures after streaming begins end
without a terminal success. Upstream status/body parity is not yet implemented.

CLI input and output both use stream-json. Input currently sends one SDK user
record plus EOF. Online evidence uses persistent process/session pools, gRPCS
and tool loops. Those remain explicit differences, not hidden compatibility claims.
Independent identity/certificates, constrained production egress, credential
refresh and CCMAX wiring are also unfinished. Never supply real credentials or
enable this route on a public/production listener.

## Verification

Default `bun test` tests use in-memory subprocess fixtures and mocked listeners.
`test/cli/roundtrip.smoke.ts` is explicit opt-in: HTTP -> actual CLI -> synthetic
loopback upstream -> JSON/SSE, plus cancellation, deadline and503. The lab helper
uses the previously reviewed base and two locked binaries without new downloads,
host installs or production access. CLI, service and stub share a network=none,
UID1000, read-only/capless/NNP container with private temporary home. This is not
an independent-instance or full network isolation acceptance test.

The stub usage1/1 is a fixture, not tokenization, model inference or billing.
Reference flags: [official CLI reference](https://code.claude.com/docs/en/cli-reference).
User record: [official streaming input example](https://code.claude.com/docs/en/agent-sdk/streaming-vs-single-mode).
Execution evidence and remaining gates live in the main verification ledger.
