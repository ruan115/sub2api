# Execution Plane protobuf policy

- `execution.v1` is additive: existing field numbers and enum values are never
  reused.
- The orchestrator supports host-agent and worker versions `N` and `N-1`.
- A peer advertises protocol major/minor and capabilities during registration.
- Unknown optional fields and capabilities are ignored; an unsupported major
  version is rejected before work is assigned.
- Plaintext credentials are never placed in control-plane commands or generic
  metadata. Secure activation uses dedicated ciphertext fields bound to the
  worker process key, slot, epoch and short-lived leases; the host-agent only
  forwards those opaque envelopes and their version acknowledgements.
- `onboarding.proto` is the internal mTLS-only CCMAX → orchestrator intake. It
  also exposes an exact intent/account/generation result lookup containing only
  bounded non-secret metadata after success, or fixed public failure/expiry
  categories after another terminal outcome. Pending remains a gRPC status and
  is never encoded as a result. Temporary request bytes must never be logged;
  intent creation returns only an opaque KMS-backed receipt.
- `RecoverOnboardingIntent` is lookup-only: `NOT_FOUND` means the exact key was
  never created, `ABORTED` means that exact intent expired,
  `FAILED_PRECONDITION` means an identity/claim/consumption conflict, and
  `UNAVAILABLE` means durable lookup could not complete. It never decrypts an
  intent or returns credential material.
- Generated Go code is produced with Buf CLI `v1.72.0`, protobuf Go plugin
  `v1.36.11` and gRPC Go plugin `v1.5.1`; generated drift is a required check.
- Worker Health adds an optional 32-lowercase-hex challenge and loaded-state
  metadata. Empty challenges retain legacy compatibility; missing loaded_state
  cannot prove readiness. The worker-local activation_revision is not the
  credential vault's numeric version. No credential bytes enter this response.
- For the message-only worker extension, run
  `sh scripts/worker-proto-offline.sh check` from `execution-plane` (`generate`
  updates it). This uses pinned, already cached Buf/Go generator sources with
  downloads disabled, and touches only `worker.pb.go`; it does not clean other
  generated output, regenerate gRPC methods or invoke remote plugins.
- NodeControl adds optional diagnostic ticket request/response events under the
  explicit `probe_tickets` capability. Only `health` and `credential_key` are
  eligible; `request_id` is fresh correlation, not authority. The response's
  execution_ticket is a bearer secret and must not be logged. Key commands add
  desired_generation; legacy commands without it cannot request new tickets.
- `sh scripts/control-proto-offline.sh check` checks only `control.pb.go` using
  the same cached tools (`generate` updates it). No gRPC method is added, and
  `control_grpc.pb.go` is unchanged.
