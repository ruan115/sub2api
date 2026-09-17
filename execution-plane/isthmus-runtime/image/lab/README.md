# S1b trusted base build lab

Read the [approved scope and plan](../../../../../openspec/changes/complete-ccmax-execution-acceptance/s1b-linux-base-build.md)
before execution. This module is deliberately pinned to one approved **shared,
idle test host**, daemon ID, systemd cgroupv2 and Linux/amd64. It is not a deployment
tool or an authorization to access production. Do not replace the daemon checks
with automatic discovery or reuse the production `isthmus-vm-base` tag.

## Responsibilities

- `host.py`: explicit local Unix daemon, empty private Docker config, environment
  allowlist, bounded subprocesses, resource checks and ownership checks.
- `packages.py`: pure exact signed-index/control/hash agreement, not APT signing
  or dependency solving.
- `run.py`, `debian.sources`: separate official acquisition and metadata phases.
- `build.py`, `buildkitd.toml`: pinned trusted builder, offline RUNs, cgroup
  observations, explicit export/load, failed-target check and final receipt.
- `smoke.py`: nonprivileged, read-only, no-network checks and every locked package.
- `cleanup.py`: only recorded exact container IDs and the owned cache volume.

Preparation runs APT in a fresh official base container, never on the host. It
has CHOWN/SETUID/SETGID/FOWNER/DAC_OVERRIDE solely for APT's `_apt` account and
private cache, no other added capabilities, no host mounts, credentials or ports,
512MiB/0.5CPU/pids128 and a finite lifetime. Signed APT indexes are fetched over
HTTP; the lock records the equivalent official HTTPS pool location. The actual
URI (including APT's literal `+` escaping), control/index fields, hashes, signed
InRelease and the real `.pgp` keyring are retained. No unauthenticated fallback.

The **rootful privileged BuildKit exception applies only to trusted official
packages**, not user workloads or real CLI/accounts. There is one fresh owned
cache volume, no Docker socket/host bind/port, no secret/SSH/build-arg/entitlement
request or inherited proxy. Limits are 2CPU/2GiB/no extra swap/pids512 and one
build at a time. A 900-second container watchdog survives client disconnection;
the client also stops the exact owned builder on cancellation/budget failure.
Disk checks stop on 8GiB host-free-space loss or less than20GiB free; this is a
monitor, not a filesystem hard quota or attribution of unrelated host writes.
Docker daemon image pulls/imports are outside the builder cgroup budget.

BuildKit moves its daemon into `/init` below the container scope. The observer
checks the exact Docker systemd **scope root** limits and every observed build
process below that root; it does not treat `/init` as the resource boundary.
RUNs explicitly use `--network=none`; registry/FROM preparation may still use
network. This is not a kernel egress policy or N3/N4 acceptance.

## Explicit phases

Use a new root-owned0700 `/var/tmp/isthmus-s1b.<random>` directory on the reviewed
host, containing only the reviewed helper source files and their `recoverykit`
imports in the repository-relative layout. Never upload the whole repository,
home, SSH config, environment files or account artifacts. The API checks the
daemon before allowing the workflow. No command below connects over SSH itself.

```text
python3 -I -B <reviewed-source>/image/lab/run.py prepare <private-lab-root>
# Review apt-plan.log and signature/update success before downloading.
python3 -I -B <reviewed-source>/image/lab/run.py download <private-lab-root>
# Review base.lock.json against signed index/control/hash evidence.
python3 -I -B <reviewed-source>/image/lab/run.py build <private-lab-root>
python3 -I -B <reviewed-source>/image/lab/run.py smoke <private-lab-root>
python3 -I -B <reviewed-source>/image/lab/run.py cleanup <private-lab-root>
```

`prepare/download` acquire a **candidate snapshot**, not a replay of an older
lock. To rebuild the committed lock, use the exact preserved approved packages
and S1a stage/verify in a new context; never silently refresh versions. The actual
S1b repeat used the same verified package bytes, a new private context and empty
builder cache, not a second live APT resolution.

Errors preserve private evidence and do not silently overwrite/retry stages.
If a phase has partially succeeded, inspect its exact recorded IDs before a
new run; cleanup is not transactional and does not claim rollback on partial
failure. A negative build must fail for the expected missing target. Its
precreated zero-byte exporter placeholder is not an image; any nonempty failure
output is rejected and never loaded. `built-image.json` is written only after
positive build, negative verification and builder stop succeed. Context receipts
remain `execution_permitted=false`, even after the image build.

Smoke runs UID/GID1000, cap-dropALL, no-new-privileges, readonly root,
networknone, private tmpfs,128MiB/0.5CPU/pids64. It checks home permissions/no
template files, empty machine-id, mount denial, all9 locked package versions/
architectures/install states and empty `dpkg --audit`. It does not initialize
persistent instance homes, generate keys, issue certificates or start a service.

Cleanup prevalidates every recorded ID/label and exact cache volume, then
rechecks ownership before each deletion. No prune, image removal or directory
deletion; exported images and source/evidence remain. Existing business container
ID/image/start time/restart count/mount metadata must remain unchanged.
