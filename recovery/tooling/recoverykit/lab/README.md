# Read-only dedicated-lab endpoint check

This is a prerequisite tool, **not** a VM implementation, image builder,
firewall installer, execution authorization or completed isolation gate.
No command starts Docker/Colima, invokes a shell/SSH, creates a container or
deletes anything. It does not call the existing `docker-e2e.sh`.

From the repository root, after explicitly clearing ambient Docker/proxy/SSH
agent variables and confirming the dedicated Engine's identity separately:

```sh
PYTHONDONTWRITEBYTECODE=1 PYTHONPATH=recovery/tooling python3 -m recoverykit lab inspect \
  --socket /absolute/private-lab/engine.sock \
  --expected-engine-id previously-confirmed-id \
  --expected-engine-name ccmax-dedicated-lab \
  --expected-architecture arm64
```

Do not use an ID discovered by the same invocation as its expected ID. The
socket must be a user-owned Unix socket with a user-owned 0700 direct parent,
no symlink ancestors, and no untrusted writable ancestors. Root-owned sticky
ancestors are allowed. Global/default sockets and remote endpoints are rejected;
the tool does not modify permissions or choose a current Docker context.

`config.py` checks inputs and socket identity; `engine.py` makes only bounded
AF_UNIX HTTP GET requests; `preflight.py` checks version, expected daemon identity
and architecture, rootful Linux/runc/security capabilities, no daemon proxy,
and an empty container/volume inventory with just the three default networks.
It uses [Engine API 1.43](https://docs.docker.com/reference/api/engine/version/v1.43/)
only if the server explicitly advertises a compatible minimum/maximum. Missing
version metadata or a newer minimum causes a failure, not a guessed fallback.
JSON field names follow the [versioned Moby types](https://github.com/moby/moby/blob/v24.0.9/api/types/types.go),
including `HttpProxy` and `HttpsProxy`, not the differently-cased Go fields.

Requests have a 3-second wall-clock deadline, the whole check has 12 seconds,
and response bodies are limited to 2 MiB. A shutdown timer also interrupts
slow-drip response headers/body: an inactivity socket timeout alone is not a
wall-clock bound. HTTP headers retain the standard library's fixed line/count
limits. Responses with redirects, unexpected framing/encoding, duplicate JSON
keys, oversized numbers or invalid shape are rejected. No raw daemon body or
exception is emitted; CLI errors contain only the error class.

Success is `docker_endpoint_checked`, with `isolation_verified=false`,
`execution_permitted=false`, and `created_resources=0`. It proves only a bounded
point-in-time metadata check against a preselected local endpoint. Docker cannot
prove the Linux VM's actual host mounts, forwarding, host firewall, current
image contents or later resource changes. Images are not inspected by this
slice; retained image/build cache is neither approved nor executed. Those checks
and a real network rejection matrix remain mandatory before any workload.
The host/daemon/current OS user are trusted; inode checks cannot defeat a
malicious same-UID process that races socket replacement or forges metadata.

Tests use synthetic documents and temporary local Unix HTTP listeners, not a
Docker daemon, CLI, production account or VM. Running these tests does not close
N3 or add points to the [delivery ledger](../../../../docs/plans/isthmus-container-delivery-v1.md).
