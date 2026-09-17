# Pinned runtime inputs (S1c)

These modules stage bytes, not accounts, instances or service releases.

- `binaries.py`: strict Bun1.4.2 candidate / Bun1.3.9 control / Claude2.1.258
  Linux amd64+arm64 lock. Each record binds an exact official HTTPS URL,
  architecture, size and SHA-256. `stage_binary(input_path, spec, dest)` does
  no networking or execution. Inputs and expanded ELF files are capped at
  256MiB each, with streaming copy, nofollow/single-link checks and new output.
  Bun ZIP metadata is bounded before parsing; only its expected ELF and an
  optional empty parent directory are accepted. No extractall or installer.
  Outer ZIP and inner ELF hashes are different fields. Successful output0755
  does not grant execution permission; a final verification failure can leave
  a file, so existence/mode alone must never substitute for success.
- `source.py`: `stage_source(repo, lock_path, dest)` reads only23 explicit Git
  blobs at commit `e2715b6e7f968e638c2f4fd68467c56fa0151c72`. No checkout,
  workspace copying, filters, dependency install or lazy network fetch. The
  result contains `source/`, `source.lock.json`, `receipt.json`; directories0700,
  files0600. It excludes tests, provenance containing local paths and home/config.
  `verify_source(dest)` checks exact inventory/bytes/permissions, not publisher
  authenticity or Git membership. Consumers must use the reviewed repository
  lock as their trust anchor, not a self-consistent lock supplied with payloads.

The23 source files total47,550 bytes and remain **fake-only**. There is no real
CLI executor, listening gRPC implementation or production app entrypoint here.
The14 pinned synthetic test inputs have their own lock and never enter the
release-source artifact. The probe uses40 exact source/test/binary paths.

## Provenance and architecture limits

Claude binary checksums/sizes were checked against its detached-signed official
manifest, with documented key fingerprint
`31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE`. The lab uses its own GPG directory,
not the user's keyring. Bun hashes were checked against official GitHub release
metadata/SHASUMS; **Bun PGP verification was not performed**. A caller-provided
hash lock alone is not a signature or a general authorization policy.

Only Linux amd64 is executed in S1c. Its actual expanded binary hashes are in
`locks/toolchain-linux-amd64-expanded-2026-09-17.json`. ARM64 has distinct official
artifact metadata, not a new native-run claim. The test host has AVX2 (required
for the selected1.3.9 x64 control);1.4.2's versioned docs specify SSE4.2 and
glibc≥2.17. Do not infer identical requirements for1.3.9 or use musl binaries
in the glibc base. ELF header checks do not establish all dynamic dependencies
or every CLI subprocess/tool path.

System/project/CI Bun remain1.3.9. The1.4.2 executable is only an explicitly
addressed candidate in the private probe, not a promoted dependency.

References: [Bun1.4.2 versioned installation notes](https://raw.githubusercontent.com/oven-sh/bun/bun-v1.4.2/docs/installation.mdx),
[Bun1.3.9 notes](https://raw.githubusercontent.com/oven-sh/bun/bun-v1.3.9/docs/installation.mdx),
[Claude release signature verification](https://code.claude.com/docs/en/setup#verify-the-manifest-signature).
