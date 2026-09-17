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
- `mtls.py`: bounded synthetic component tests in one UID1000/network-none
  container; default profile covers preinstalled instance TLS.
- `enrollment.py`: opt-in nine-suite profile of that same sandbox, covering
  authenticated issuance, pre-listener local install and actual worker/Controller
  TLS. Docker bootstrap transport and SQL use mocks; no cross-container, CLI
  bridge, restricted egress or production acceptance is claimed.
- `livebootstrap/`: separate two-worker Docker management probe, split into
  artifact verification, container policy, bounded coordinator IPC and cleanup.
  Read the [S2b2-live plan](../../../../../openspec/changes/complete-ccmax-execution-acceptance/docker-enrollment-s2b2-live.md)
  first: a trusted host coordinator may use the daemon socket, and workers have
  one hash-pinned public binary file mounted read-only. This lab-only exception
  does not relax production provider checks. Keys remain in private container
  tmpfs; no real credentials, network, cross-container mTLS or CLI bridge.

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
# S1c toolchain probe extension

`toolchain.py acquire|payload|probe <private-root>` is a separate manual phase
entrypoint on the same exact approved daemon. It retains the `isthmus-s1b.<run>`
private-root guard; that naming does not imply another base build. Use a fresh
root for a rerun and the explicitly reviewed source-file upload list, never a
whole repository or account directory. On cross-machine tar transfers use
`--no-same-owner` into a new root-owned directory: macOS UID501 must not become
the owner of a Linux release bundle. Do not replay over partial evidence.

1. Locally call `artifacts.source.stage_source` with the reviewed app lock and
   `lab.payload.tests_from_git` with the explicit repository path. Upload only
   their private outputs as `frozen-source` / `frozen-tests` plus reviewed helper
   modules, required recoverykit evidence helpers and public locks.
2. `acquire` fetches only fixed official public artifacts. Claude's detached
   manifest signature/key fingerprint and both architecture hashes/sizes are
   checked before amd64 downloads. Bun PGP is not asserted. No ambient proxy,
   installer, host installation, API credentials or unbounded download; POSIX
   main-thread SIGALRM enforces each300s download deadline and restores handlers.
3. `payload` copies40 paths bound to repository-reviewed locks:23 source,
   14 synthetic tests,3 binaries. Root-owned0755/0644 public copies are inside
   an enclosing0700 task directory, separate from private release-source output.
4. `probe` first checks exact inventory/hashes, AVX2 and host memory reserve.
   It creates the fixed S1b image ID with UID1000/dropALL/NNP/networknone,2GiB
   memory and swap=memory,1CPU,pids128,core0,no host mounts or ports. Root is
   read-only; private home and/tmp are writable noexec. Only the512MiB
   `/opt/isthmus-probe` tmpfs is executable. A bounded stdin upload feeds a
   generated40-file USTAR to fixed system shell/tar as UID0, still capless/NNP
   and networknone (checked before extraction). No external tar, links, PAX or
   new binary executes as root. UID1000 must prove the resulting root-owned
   tree is unwritable before execution. Docker29 rejects cp into readonly
   rootfs even for tmpfs destinations, so Docker cp is not used.
5. PID1 is only a240s timeout/sleep watchdog. The test command has its own120s
   watchdog,150s client deadline and bounded logs. It checks actual kernel
   cgroup/mount options, nonwritability and all40 hashes before CLI--version,
   Bun offline tests and synthetic loopback gates. Networknone forbids upstream
   traffic even though local loopback sockets work. No model requests.
6. Finally stop/remove only the verified owned ID and compare original container
   metadata; only then publish a successful result. Keep failures/logs for
   diagnosis, no prune/image/volume/business deletion. A lost SSH client is
   bounded by PID1; reconnect to inspect and remove any stopped owned container.

This is not a composed immutable runtime image, a general workload sandbox,
an I3/I4/I5 or N3 acceptance shortcut. Signature/hash checks do not make copied
home directories safe. Do not add real accounts or extra mounts to this probe.
