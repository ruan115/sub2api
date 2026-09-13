## ADDED Requirements

### Requirement: Explicit local demo boundary

The Portunex demo SHALL expose only synthetic data under a dedicated development prefix, bind to loopback, and SHALL NOT register production routes or contact external systems.

#### Scenario: Production-looking request

- **WHEN** an old `/portunex` client or a non-loopback request reaches the demo
- **THEN** it is not silently handled as a compatible production service.

### Requirement: Server-side role and ownership enforcement

The demo SHALL reject anonymous and expired sessions and enforce role/owner checks before listing data. Cookies SHALL NOT expose the token to JavaScript.

#### Scenario: Member opens an admin list

- **WHEN** a member requests users or providers
- **THEN** the server returns forbidden even if the frontend guard is bypassed.

### Requirement: Shared fake turn core

HTTP and WebSocket adapters SHALL use one fake turn interface with bounded streaming and cancellation, without launching the recovered bundle or real CLI.

#### Scenario: Cancellation during streaming

- **WHEN** the client cancels an active turn
- **THEN** work terminates, resources are released, and a reusable WebSocket may accept a subsequent turn.

### Requirement: Honest evidence status

Synthetic tests SHALL NOT upgrade unobserved old API behavior to verified compatibility.

#### Scenario: Remote source unavailable

- **WHEN** source acquisition fails
- **THEN** missing contract fields remain unknown and the blocker is recorded independently of demo progress.
