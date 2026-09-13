# Synthetic PostgreSQL capability for integration tests

`Start(t)` exists only with `portunex_integration` on macOS/Linux. It accepts no
DSN, existing cluster directory, Docker context or production settings. The only
runtime input is an explicit `PORTUNEX_TEST_PG_BIN` directory; binaries must be
ordinary executable files reached without symlinks. The pinned build recipe is
in `recovery/runtime/postgres/` (server version is additionally checked at startup).

Every call initializes a fresh 0700 temp directory, local-trust/host-reject HBA,
no TCP listener, a unique private Unix socket and a random synthetic database.
Subprocesses receive only PATH/locale/timezone, not HOME/PG/DYLD/LD configuration.
The Go connector rejects inherited PG/database routing variables before pq parses
them, sets explicit options and a nonempty synthetic password to bypass .pgpass,
and uses a dialer that can only open this instance's exact Unix socket.

Before applying any recovery SQL it verifies version 18.6, database/user,
data_directory, empty listen_addresses, exact unix_socket_directories, a NULL
TCP address, and a per-instance marker. It does not install citext or migrations:
each caller applies `migrations.SQL()` in an explicit transaction to its own DB.
It does not import or invoke the existing Sub2API integration harness.

`Close` and t.Cleanup operate only on the instance they created. On successful
normal fast shutdown, PostgreSQL waits for children and the helper reaps the
postmaster before removing that new directory. Bootstrap failure, unexpected
exit, timeout/forced shutdown, or connection-close failure are **errors** and
retain data. A forced parent exit is not treated as complete child cleanup;
PostgreSQL children can create their own sessions. No global process kill is used.

Tests cover the ordinary real lifecycle and pure rejection/retention decisions.
They do not deliberately SIGSTOP real PostgreSQL children or claim that every
OS-level failure is recoverable. A retained abnormal instance needs explicit
owned-process inspection; never delete an uncertain live data directory.
