# Execution Plane protobuf policy

- `execution.v1` is additive: existing field numbers and enum values are never
  reused.
- The orchestrator supports host-agent and worker versions `N` and `N-1`.
- A peer advertises protocol major/minor and capabilities during registration.
- Unknown optional fields and capabilities are ignored; an unsupported major
  version is rejected before work is assigned.
- Credentials are never placed in control-plane commands. A worker receives a
  short-lived, single-use credential lease only after its slot ticket and epoch
  have been verified on the authenticated worker channel.
- Generated Go code is produced with Buf CLI `v1.72.0`, protobuf Go plugin
  `v1.36.11` and gRPC Go plugin `v1.5.1`; generated drift is a required check.
