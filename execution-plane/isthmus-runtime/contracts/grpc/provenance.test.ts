import { expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";

const SOURCE_SHA256 =
  "c582535f7620ea07a7a2de5957f50903a97eabe13579ce739127ff9e6329a369";

test("the canonical proto retains the exact bytes recovered from the source", () => {
  const bytes = readFileSync(new URL("./messages.proto", import.meta.url));
  const provenance = JSON.parse(
    readFileSync(new URL("./provenance.json", import.meta.url), "utf8"),
  );
  expect(provenance.schema_version).toBe(1);
  expect(provenance.artifact.path).toBe("messages.proto");
  expect(provenance.artifact.size_bytes).toBe(4433);
  expect(bytes.byteLength).toBe(provenance.artifact.size_bytes);
  expect(provenance.artifact.sha256).toBe(SOURCE_SHA256);
  expect(createHash("sha256").update(bytes).digest("hex")).toBe(SOURCE_SHA256);
});
