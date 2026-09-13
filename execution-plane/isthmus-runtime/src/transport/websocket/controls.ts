import { CLIENT_FRAME_TAGS as TAG } from "../../protocol/websocket";
import { TurnError } from "../../runtime/turn/errors";
import { usageLimit } from "../../runtime/turn/request";
import type { TurnControls } from "../../runtime/turn/types";

const TOKEN = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;
export const MAX_CONTROL_BYTES = 8192;
const MAX_TOKENS = 128;

/** Metadata is for the next REQUEST only. Limits are fake-service policy. */
export class PendingControls {
  private value: TurnControls = {};
  private bytes = 0;
  private tokens = 0;

  accept(tag: number, payload: Uint8Array): void {
    if (payload.length + this.bytes > MAX_CONTROL_BYTES) {
      this.clear();
      throw new TurnError(413, "request_too_large", "Pending control metadata exceeds the fake service limit");
    }
    this.bytes += payload.length;
    const text = new TextDecoder().decode(payload);
    if (tag === TAG.USAGE_LIMIT) {
      const token = usageLimit(text);
      if (token !== undefined) this.value = { ...this.value, usageLimit: token };
      return;
    }
    const tokens = text.split(",").map((token) => token.trim()).filter((token) => TOKEN.test(token));
    if (this.tokens + tokens.length > MAX_TOKENS) {
      this.clear();
      throw new TurnError(413, "request_too_large", "Too many pending beta tokens");
    }
    this.tokens += tokens.length;
    const key = tag === TAG.BETA_REPLACE ? "replaceBeta" : tag === TAG.BETA ? "appendBeta" : "removeBeta";
    if (tokens.length > 0 || tag === TAG.BETA_REPLACE) {
      this.value = { ...this.value, [key]: [...(this.value[key] ?? []), ...tokens] };
    }
  }

  take(): TurnControls {
    const result = this.value;
    this.clear();
    return result;
  }

  clear(): void {
    this.value = {};
    this.bytes = 0;
    this.tokens = 0;
  }
}
