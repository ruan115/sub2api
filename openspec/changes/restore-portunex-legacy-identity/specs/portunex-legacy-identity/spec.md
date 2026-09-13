## ADDED Requirements

### Requirement: Bounded password verification in a separate recovery module
The implementation SHALL keep password parsing and verification separate from the synthetic demo, validate cost before KDF execution, reject unsupported encodings without fallback, and retain a concurrency slot until computation finishes.

#### Scenario: Cancellation during password computation
- **WHEN** the caller cancels a request while a KDF is running
- **THEN** the computation retains its slot until completion and the caller receives a cancellation error rather than permission to create a session

### Requirement: Evidence does not imply runtime compatibility
The recovery SHALL distinguish static literal evidence, local implementation policy, and behavior verified against an isolated original implementation.

#### Scenario: Only an algorithm dependency name is found
- **WHEN** an original executable contains an algorithm name but control flow is not proven
- **THEN** the algorithm remains a candidate and no legacy production route is enabled on that basis

### Requirement: Synthetic database isolation
The test system SHALL use a separately provisioned local database containing only synthetic records; it SHALL NOT reuse production data, production DSNs, or the existing Sub2API migration harness.

#### Scenario: Local PostgreSQL is unavailable
- **WHEN** no isolated PostgreSQL runtime is available
- **THEN** unit checks may proceed while real PostgreSQL verification is explicitly reported as unverified
