# isthmus-vm-base: offline base-image build inputs

This module implements **S1a**, the base-only recipe and private build-context
staging. It does not download, build, start Docker/Colima, invoke a shell, or
execute/unpack packages. The current runtime is still fake-only. This image
recipe has **not been built**, and there is no committed complete release lock.
An image called `isthmus-vm-base` is not by itself a working isthmus service.

## Files and responsibility

- `Dockerfile.base`: reviewed template, fixed platform/digest substitutions;
  offline package installation, UID/GID 1000, private empty home, default
  `/bin/false`. No `SYS_ADMIN`/setcap, dynamic installer, implicit volume,
  credential, instance key, or listener. The literal template is not a build
  input until rendered; placeholders intentionally prevent casual use.
- `imagekit/lock.py`: exact reviewed-input schema, limits and source policy.
- `imagekit/recipe.py`: deterministic recipe/checksum rendering, no build args.
- `imagekit/context.py`: bounded file copy and private context verification.
- `stage.py`: explicit paths and fixed JSON summary/error output.
- `test/`: synthetic locks and ar-magic bytes, not installable Debian packages.

The old online base did not contain app/Bun/CLI either. Those artifacts belong
to a later derived runtime image, not mutable shared application volumes. Their
versions/hashes, architecture dependencies and actual startup remain I3/I5/R
gates. This changes packaging, not the caller's HTTP/WS/gRPC contracts. It does
not prove equivalent nested CLI process isolation; that requires S2/S4 testing.

## Explicit reviewed lock

Input JSON must have exactly `schema_version: 1`,
`kind: "isthmus-base-build-inputs"`, `platform: "linux/amd64"` or
`"linux/arm64"`, `base_image: "docker.io/library/debian@sha256:<64 lowercase hex>"`,
and `packages`. Each package has exactly:

| Field | Required meaning |
| --- | --- |
| `name`, `version` | Debian package name and explicit version, optional epoch |
| `architecture` | `all` or the selected platform architecture |
| `file` | `<name>_<version without epoch>_<architecture>.deb`, flat basename |
| `size`, `sha256` | Exact bytes and lowercase SHA-256, not copied from a filename |
| `source_url` | Explicit HTTPS Debian main-pool URL whose basename matches |

Allowed origins are `deb.debian.org/debian`,
`security.debian.org/debian-security`, and timestamped
`snapshot.debian.org/archive/debian[-security]`. URLs are recorded only, never
fetched; credentials, query/fragment, ports, redirects, traversal and encoded
paths are not accepted. No `latest`, mutable base tag or image config ID in
place of the registry digest. Limits: 128 packages, 64 MiB/package, 256 MiB total,
256 KiB metadata. Core packages: `ca-certificates`, `passwd`, `procps`,
`util-linux`; include all other necessary dependencies explicitly. Privilege
or daemon packages outside this base scope are rejected.

The lock is trusted, human-reviewed input, **not a signature verifier**. Hashes
check consistency with that input; a malicious lock can describe malicious
bytes. The minimal ar header check does not validate Debian control metadata,
dependency closure, maintainer scripts, architecture, publisher signature or
package installability. Those require verified official release/index evidence
and actual isolated builds. No fabricated production versions/digests are
provided to make an incomplete dependency set look ready.

## Local staging and verification

From the repository root, with an already reviewed complete lock and approved
local packages (replace the explicit absolute paths):

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 \
  execution-plane/isthmus-runtime/image/stage.py stage \
  --lock /absolute/reviewed/base.lock.json \
  --source-root /absolute/reviewed/packages \
  --destination /absolute/private-parent/new-base-context

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 \
  execution-plane/isthmus-runtime/image/stage.py verify \
  --directory /absolute/private-parent/new-base-context
```

Only the explicitly listed flat package files are copied, never an entire
directory or repository. Symlinks, special files and hardlinks are rejected;
size/hash and file identity are checked around bounded reads. The destination
must be new, outside the source and Git worktrees, with an existing parent.
Directories are 0700 and files 0600. Preflight/copy failures do not publish a
receipt. Failure during the final receipt write or verification may leave a
receipt file, but never a successful command result: its existence alone does
not imply completion. Independently verify before any use. Failed output is not
automatically cleaned or overwritten; investigate before using a new destination.

The result contains `Dockerfile`, `packages.sha256`, `packages/`, the canonical
`artifacts.lock.json`, and a final `receipt.json`. Verify checks the exact file
and directory inventory, permissions, canonical lock, rendered recipe,
checksums and receipt. This detects inconsistency, not malicious same-user
rewriting of all trusted inputs. Keep the host/user and reviewed recipe trusted.
CLI errors expose only an exception class. Environment proxy/Docker settings
are never consulted; staging does not connect to any daemon.

Even success returns `image_built=false` and `execution_permitted=false`.
`lab inspect` success is not build authorization. A later explicit dedicated
builder must check VM mounts/forwarding, daemon identity, base availability,
platform and network rules. Per the [Dockerfile reference](https://docs.docker.com/reference/dockerfile/),
`RUN --network=none` controls RUN networking; it cannot establish that FROM or
the builder itself made no network requests. No external syntax frontend is
selected by this recipe. Only the staged [build context](https://docs.docker.com/build/concepts/context/)
may be sent to that future builder, never the repository root or account home.

Real builds must additionally verify dpkg dependency/Pre-Depends ordering,
base platform/contents, package identity and absence of inherited capabilities
or identity material. dpkg failure has no network repair fallback. Removing a
machine-id from this template is not an implementation of per-instance identity;
K1–K5 and home persistence remain separate gates. Do not start this base as a
worker or weaken provider restrictions to make it run.

## Verification

```sh
make -C recovery image
```

Only synthetic offline tests. They cannot close I2/I3/I5, N3 or production
compatibility. Stage mapping and progress reporting:
[focused stages](../../../../openspec/changes/complete-ccmax-execution-acceptance/isthmus-focused-stages.md),
[fixed ledger](../../../../docs/plans/isthmus-container-delivery-v1.md).
