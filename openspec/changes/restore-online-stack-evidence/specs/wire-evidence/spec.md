## ADDED Requirements

### Requirement: Source-bound observations

The offline validator SHALL verify every observation anchor against an explicitly allowlisted artifact and its exact UTF-8 byte range, and SHALL reject mismatching hashes or unknown catalog references.

#### Scenario: Mutated source

- **WHEN** the source bytes no longer match the artifact manifest or anchor hash
- **THEN** validation fails without displaying source content.

### Requirement: No semantic promotion

Successful source anchoring SHALL NOT automatically change catalog status, certify an observation's semantics, or claim business compatibility.

#### Scenario: Valid synthetic observations

- **WHEN** all references and bytes validate
- **THEN** the result reports source anchoring only and business verification remains false.

### Requirement: Bounded no-follow catalog reads

The catalog dependency SHALL reject symbolic links at every ancestor and catalog component, use bounded regular-file reads, and reject files changed during reading. Catalog manifests SHALL be limited to 1 MiB each, 16 MiB in aggregate, and 256 manifests.

#### Scenario: Catalog manifest replaced by a link

- **WHEN** a manifest is replaced with a symbolic link after directory inspection
- **THEN** the validator fails without reading the link target or reporting source anchoring.
