# Legacy wire evidence

## Requirement: named static artifacts only

The recovery pass SHALL retrieve only explicitly reviewed static filenames into
a new private directory outside Git, compare local size/SHA-256 to read-only
remote metadata, and never execute the original scripts.

### Scenario: a collected file fails the secret-shaped-content policy

The original SHALL remain private and excluded from the usable artifact manifest.
The pass SHALL record the exclusion without exposing the matching value or
weakening policy. Other independently screened files may be used, but statements
that require the excluded source SHALL remain unknown.

## Requirement: preserve source and business evidence levels

Each committed observation SHALL refer to a known API record and exact path,
verified artifact identity and original UTF-8 byte-span hash. Full byte checks
SHALL require the private originals; CI metadata tests SHALL NOT claim to perform
that check or authenticate server behavior.

### Scenario: client code shows Bearer authentication

The plan SHALL distinguish the observed token/localStorage/Bearer chain from the
synthetic cookie-session demo. Prefix checks, UI role gates and consumed fields
SHALL NOT be treated as complete server authorization or DTO specifications.

## Requirement: bounded catalog-only identity inventory

The collector SHALL use an explicit READ ONLY transaction, pg_catalog search path,
short statement/lock timeouts and a rollback. The scope SHALL be public catalog
counts and the columns of users/auth_sessions/api_keys, without business rows,
secret values, default expressions, function bodies or sequence current values.

### Scenario: column names reveal token or key fields

The inventory SHALL record names/types only and SHALL NOT claim the values are
plaintext, hashed or encrypted. Independent query and normalized-output hashes
SHALL bind the reviewed artifacts; schema_restore_verified SHALL remain false.

## Requirement: no implicit production implementation

The pass SHALL leave original discovery history, production enable flags,
existing demo behavior and Bun version unchanged. Unknown PHC/token/ID/response
rules SHALL be explicit prerequisites of the next compatible-auth plan.
