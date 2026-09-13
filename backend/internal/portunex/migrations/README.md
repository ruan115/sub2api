# Identity recovery schema

This module is **only the explicit schema for a fresh, local synthetic recovery
database**. It is not the original SQLx migration history, a complete Portunex
database, or a production migration runner. Nothing executes on package import.
The caller must execute `SQL()` in one explicit transaction, rolling back on any
error. There is deliberately no `Apply` helper or `IF NOT EXISTS`: an existing
schema or extension is a conflict, not evidence that its definition is correct.

Source: `recovery/baselines/portunex/postgres/identity-definition.json`, collected
read-only from PostgreSQL 18.6. The three tables preserve the observed 30 columns,
defaults, NULL semantics, checks, foreign keys, and 17 indexes (including three
primary-key indexes). Tables and foreign-key targets move from `public` to the
fixed `portunex_identity` schema. `citext` remains explicitly `public.citext` at
observed extension version 1.8. Ownership, grants, production locale/collation,
other tables, and the original migration history are not reconstructed here.

Notably, bigint IDs have no default or generator; session expiry has no default;
API-key text and user email remain nullable; active-value uniqueness applies
only where `deleted_at IS NULL`; and soft deletion does not cascade. No token
encoding, hashing, TTL, refresh, API-key policy, or login behavior is inferred.

Ordinary tests compare DDL text to committed catalog evidence. They do not prove
PostgreSQL semantics. Tests behind `portunex_integration` use the independent
`platform/postgres/testcluster` helper and its explicit local runtime, fail when
that runtime is unavailable, and exercise a fresh synthetic database only.
