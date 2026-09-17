# Authenticated existing-instance START

`New` composes one provider with the original authenticated enrollment
coordinator, host controller and slot-command executor. It does not enable
`cmd/host-agent`, create a control channel, issue tickets, register a business
runtime, or grant production readiness. The daemon wiring remains a separate
gate; do not replace it with a fake activation or signer.

## Responsibilities

- `composition.go`: validate dependencies and construct the strict command path;
  no provider side effects during construction, no caller-supplied startup or
  alternate bootstrap provider.
- `startup.go`: `StartExisting` and close the short-lived authenticated connection.
- `../runtime_existing.go`: pin immutable physical `RuntimeID`, validate complete
  account/sandbox/resource/network spec before starting, bootstrap before
  readiness, establish actual mTLS and revalidate the same instance afterward.
- `../command_start_proof.go`: prevent raw provider/TCP health from reviving a
  failed or never-authenticated START. This process-local evidence is neither a
  fresh connection check nor an execution lease. A new process must reauthenticate.
- `../../provider/docker/existing.go`: read-only complete-spec validation of one
  exact physical container, with the old logical `ProviderRef` kept separate.

START never creates/recreates an absent instance. A failure does not stop or
delete an instance whose ownership/lifetime might have changed; the existing
authorized lifecycle must decide cleanup. No activation or business/health
ticket is consumed. CREATE and other lifecycle commands retain their existing
behavior; strict START is not a general authority for them. Successful START is
a point-in-time authenticated transport observation, not credential activation.

Use a real authenticated `bootstrap.EnrollmentClient` on the current node
control session, its trust and node certificate. A component/fake provider is
not production Docker evidence. The legacy nil-Startup constructor path is
retained for existing components only; new composition must use this package.
