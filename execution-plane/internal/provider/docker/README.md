# Docker instance adoption boundary

This provider implements one Docker/runc account slot, not a separate-kernel
hypervisor VM. The host, Docker daemon and shared kernel are trusted.

`sandbox.go` is the read-only, point-in-time gate shared by existing-instance
Create, Inspect, InspectSlot, Start, RuntimeEndpoint, ValidateExisting and
bootstrap pre/post-exec verification. It verifies:

- canonical instance labels, hostname, non-root UID/GID and bootstrap identity;
- immutable Config.Image resolved through the Engine to the actual image ID
  (registry manifest digests and local config IDs are different identifiers);
- exactly one owned internal IPv4 bridge, matching endpoint/gateway/member and
  no other members or published ports;
- read-only root, capability drop with no additions/privileged mode, approved
  runtime/security profiles, private namespace settings and bounded resources;
- explicit `MemorySwap == Memory > 0`, both requested at creation and checked
  at adoption; missing/null/zero/unlimited or unequal swap ceilings fail closed;
- safe anonymous tmpfs only, no host/named-volume/device mounts, and explicit
  credential-free egress environment.

Reusing a slot additionally matches the requested account hash, epoch, image,
proxy, UID, security profiles and resource/tmpfs values. Start uses the inspected
container ID, not a potentially replaced name. Unexpected state is rejected;
inspection does not pull, repair, disconnect, remove or upgrade it. Older
containers missing the new bootstrap/inspect requirements are not silently
adopted. Their migration needs an explicit lifecycle operation after validation.
Configuration rejects empty/malformed or `unconfined` profile allowlist entries;
the provider copies the validated slices so later caller mutation cannot change
that policy. Direct Stop, Drain and Destroy are not sandbox-adoption gates;
this change does not widen or automatically invoke those cleanup operations.

Docker interprets `MemorySwap` as the combined memory-plus-swap ceiling. Equal
positive `Memory` and `MemorySwap` prevent swap; zero means unset and -1 means
unlimited, not disabled. See [Docker's memory-swap documentation](https://docs.docker.com/engine/containers/resource_constraints/#--memory-swap-details).
This provider checks the Engine policy, not whether a particular kernel/cgroup
enforced it. Dedicated Linux cgroup verification is still required; a host with
no swap configured or container `free` output is not that evidence. Old containers
without explicit no-swap policy are rejected without updates, recreation or
cleanup. There is no opt-out switch or host swap/sysctl modification.

Tests use explicit typed Engine JSON fixtures, including both projected and
omitted tmpfs Mounts and created-but-not-started network state. Negative cases
first check the same positive fixture and verify rejection does not mutate
Docker. The fixture shape still requires verification against a dedicated real
Linux Engine; compile-only Docker tests are not evidence of runtime success.
The [Moby create path](https://github.com/moby/moby/blob/master/daemon/create.go)
parses security options during creation, but source inspection and daemon
metadata are not proof that a particular host kernel enforced those settings.

This gate cannot prevent changes after inspection, enforce host-port/DNS/IPv6
firewall rules, or protect secrets against a malicious host/kernel. The shared
`provider.ValidateEgressProxyURL` contract configures the worker's application
transport; it is not a kernel egress ACL. Per-VM machine identity, private-key
provisioning, RPC mTLS, revocation and current composed-chain tests remain
separate gates in the [VM plan](../../../../openspec/changes/complete-ccmax-execution-acceptance/vm-isolation-design.md).
