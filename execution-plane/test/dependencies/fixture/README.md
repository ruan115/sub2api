# Private integration dependency fixtures

This preparation-only tool generates credentials and initialization SQL for a
**new, disposable, isolated MySQL 8 / Redis instance**. It never connects to a
database or starts a service. Do not use its output against production, existing
databases, shared Redis, or an existing MySQL data volume.

From `execution-plane/`, choose a new output directory **outside the repository**:

```sh
go run ./test/dependencies/fixture \
  --output /absolute/private-parent/new-fixture-directory \
  --ccmax-schema-source ../ccmax-manager/execution_outbox.go \
  --mysql-port 33379 --redis-port 63979
```

The parent must already exist. The output directory is mode `0700`; all five
files are `0600`. Existing output directories are rejected, including empty ones.
Passwords are independently generated from 32 random bytes and never printed.
Keep every generated file out of Git, logs and chat. Partial output from a failed
write is retained for inspection; it is never silently overwritten.

- `root-password`: MySQL root secret, suitable for `MYSQL_ROOT_PASSWORD_FILE`.
- `admin.cnf`: local MySQL admin client option file; initialization only.
- `init.sql`: embedded runtime migrations, the exact CCMAX outbox/consumer and
  commit-lock statements extracted from the named source function, and separate
  DML-only accounts for `isthmus_p5_runtime` / `isthmus_p5_ccmax`.
- `redis.conf`: authenticated, nonpersistent Redis with a 64 MiB no-eviction cap.
- `test.env`: only the three integration-test connection variables, targeting
  `127.0.0.1` at the specified ports. Source privately; never use `set -x`.

**This is not a complete CCMAX business schema.** In particular, it does not
create `accounts` or satisfy the complete orchestrator CCMAX schema preflight.
It supports the existing runtime-store, outbox-source and Redis integration
tests; their passing does not establish the cross-component custody lifecycle.

Publish dependency ports on host loopback only, with a dedicated network and data
volumes. Redis binds its container interfaces because host loopback publication
must reach the container; this is not permission to expose its port publicly.
Containers running as a non-root UID may need a separate, carefully scoped
ownership step for their private mounted config; do not make credentials public.
Apply initialization once to a fresh MySQL volume: runtime migrations intentionally
contain non-idempotent CREATE/ALTER statements. Outbox must start empty and its
integration test must not run concurrently against the same database.

Run the Redis integration on the same machine as Redis: its 100 ms lease test can
fail spuriously over a high-latency SSH tunnel. Tests may be cross-compiled locally
for Linux and executed remotely without replacing the remote Go toolchain.
