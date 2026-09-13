# Local password verification capability

This package is **not a recovered Portunex login implementation**. Argon2-related
names in the original ELF are static candidates, not proof that the old login
path used Argon2id, a particular version, PHC encoding, or particular parameters.
Do not connect this module to legacy HTTP routes or real records until those
contracts are independently established. The synthetic demo remains separate.

The only implemented format is `$argon2id$v=19$m=...,t=...,p=...$salt$hash`.
The three parameter keys may be reordered; each must occur exactly once. Integers
must be positive canonical decimal uint32 values (parallelism also fits uint8).
Salt and hash use canonical, unpadded standard Base64 with no whitespace. Unknown
algorithms, versions, parameters, padding, extra fields and malformed inputs fail
closed. This is a declared local capability, not a claim of legacy acceptance.

`NewVerifier(Limits)` requires an explicit local policy; there are no default
parameters. Limits bound input length, salt/hash length, declared memory KiB,
passes, parallelism, memory-times-passes work and simultaneous KDFs. Argon2's
minimum `memory >= 8*parallelism` is checked before its implementation can round
memory. Parameters are never truncated or upgraded. Configure the policy for the
host: simultaneous KDF memory is at most `MaxConcurrent * MaxMemoryKiB`, plus
runtime overhead and completed allocations awaiting garbage collection. This is
not an OS-enforced RSS limit or an exact wall-clock deadline.

Verification uses existing `golang.org/x/crypto/argon2` and `x/sync/semaphore`.
Full capacity immediately returns `ErrBusy`; there is no waiting queue. The KDF
runs synchronously and cannot be canceled halfway through. Its slot is released
only after work finishes. A canceled context never returns success once observed;
callers must also check cancellation before any later session side effect.
Returned errors contain no password, PHC, salt, hash or token. The temporary
password byte copy and derived result are cleared, but this does not promise
zeroization of caller strings or the KDF's internal memory.

Tests use public upstream vectors and synthetic data only. Small test costs are
for correctness and scheduling tests, not recommendations for production hashes.
The unexported KDF function slot is used by same-package tests to deterministically
exercise admission/cancellation; no production injection option is exported.
