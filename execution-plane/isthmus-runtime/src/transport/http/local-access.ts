import { TurnError } from "../../runtime/turn/errors";

/** Local-demo policy, not recovered production authentication or CORS behavior. */
export function enforceLocalAccess(request: Request, hostname: "127.0.0.1" | "::1", port: number): void {
  const address = hostname === "::1" ? "[::1]" : hostname;
  const expected = new URL(`http://${address}:${port}`);
  const url = new URL(request.url);
  const host = request.headers.get("host") ?? url.host;
  const match = /^(127\.0\.0\.1|\[::1\])(?::([1-9][0-9]{0,4}))?$/.exec(host);
  const hostPort = match?.[2] ? Number(match[2]) : 80;
  if (!match || match[1] !== address || hostPort !== port || url.origin !== expected.origin || url.username || url.password) {
    throw new TurnError(403, "permission_error", "Fake listener requires its exact loopback host and port");
  }
  const origin = request.headers.get("origin");
  if (origin !== null && origin !== expected.origin) {
    throw new TurnError(403, "permission_error", "Fake listener rejects cross-origin requests");
  }
}
