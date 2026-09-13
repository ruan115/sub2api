// Package migrations holds an explicit local recovery schema, not a production
// migration runner. It does not connect to a database or apply SQL on import.
package migrations

import _ "embed"

//go:embed 001_identity.sql
var identitySQL string

// SQL returns the local synthetic identity schema. Callers must use a fresh,
// isolated database and execute all of it in one explicit transaction. Existing
// schemas or citext installations cause an error rather than being accepted or
// overwritten. The caller owns commit/rollback; no automatic Apply is exposed.
func SQL() string { return identitySQL }
