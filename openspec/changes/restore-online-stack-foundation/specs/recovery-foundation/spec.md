## ADDED Requirements

### Requirement: Feature-scoped directory ownership

Recovery tools, Portunex business contracts and runtime protocol code SHALL occupy separate module directories with explicit dependency direction. Existing production/WIP files SHALL NOT be moved to manufacture this structure.

#### Scenario: New feature module

- **WHEN** a feature is added
- **THEN** its domain/transport/repository and tests belong to that feature rather than a global monolithic service file.

### Requirement: Safe evidence preservation

The preservation tool SHALL verify declared size/hash, reject unsafe paths/symlinks and sensitive targets, and write only to a new private destination. It SHALL never execute evidence.

#### Scenario: Tampered or unsafe input

- **WHEN** any manifest entry is invalid, missing, changed or escapes its root
- **THEN** preservation fails and does not report a valid snapshot.

### Requirement: Private WIP snapshot

A workspace snapshot SHALL preserve the Git identity, tracked patch and untracked content with integrity metadata without changing the source worktree or restoring anything automatically.

#### Scenario: Dirty execution branch

- **WHEN** the user has existing modifications
- **THEN** the original files, index and branch remain unchanged and the private snapshot can be verified independently.

### Requirement: Honest compatibility status

A contract SHALL carry evidence, owner and explicit unknowns. Discovery SHALL NOT be reported as implemented or behavior-verified.

#### Scenario: Only a route is known

- **WHEN** a frontend asset proves a path but not its complete method/authorization/body contract
- **THEN** the item remains discovered and records the missing fields.

### Requirement: Isolated protocol foundation

The isthmus WS frame codec SHALL preserve payload bytes and validate frame direction/tag/size without creating sockets, processes or upstream requests. The Messages proto SHALL retain its original wire identity.

#### Scenario: Frame roundtrip

- **WHEN** a valid known frame is encoded then decoded
- **THEN** tag and payload are preserved and wrong-direction/unknown/oversized frames are rejected.
