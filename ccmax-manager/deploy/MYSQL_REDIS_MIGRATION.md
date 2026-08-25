# MySQL and Redis migration

CCMAX uses MySQL as the durable source of truth. Redis is limited to ephemeral
runtime coordination. Billing, API keys, accounts, authorization state, and
audit logs must never depend on Redis persistence.

## Runtime configuration

Keep the datastore values in a root-owned file that is readable by the CCMAX
service group:

```ini
CCMAX_MYSQL_DSN=ccmax:password@tcp(127.0.0.1:3306)/ccmax?charset=utf8mb4&collation=utf8mb4_unicode_ci&timeout=5s&readTimeout=30s&writeTimeout=30s
CCMAX_REDIS_ADDR=127.0.0.1:6379
CCMAX_REDIS_PASSWORD=replace-me
CCMAX_REDIS_DB=0
```

The service must load this file with `EnvironmentFile=`. MySQL and Redis should
listen on loopback only. Redis should use `noeviction`; an exhausted runtime
store must fail visibly instead of silently dropping rate-limit state.

## Baseline import

Create a consistent SQLite backup. Under sustained writes, pause the single
SQLite writer process while `.backup` runs; otherwise the SQLite backup API can
restart page copies indefinitely.

Run the importer against the backup with:

```bash
CCMAX_MIGRATE_FROM_SQLITE=/path/to/ccmax-snapshot.db \
CCMAX_MIGRATE_RESET_TARGET=1 \
/opt/ccmax-manager/ccmax-manager
```

The reset flag is only valid for a disposable shadow target. The importer
checks every source column is mapped, validates per-table row counts, and
compares billing and token aggregates before it exits successfully.

## Incremental catch-up

After the baseline import, create another consistent SQLite backup and run:

```bash
CCMAX_MIGRATE_FROM_SQLITE=/path/to/ccmax-final.db \
CCMAX_MIGRATE_INCREMENTAL=1 \
/opt/ccmax-manager/ccmax-manager
```

Append-only log tables copy IDs above the MySQL watermark. Mutable tables are
upserted, then stale rows are removed in reverse dependency order. The same row
count and aggregate checks run after the catch-up.

Do not run a shadow API against the target between the baseline and final
catch-up. Shadow writes can advance append-only IDs and invalidate the
watermark.

## Cutover

1. Stop new requests or wait for a maintenance window. Do not terminate active
   streaming requests just to switch databases.
2. Create the final consistent SQLite backup.
3. Run the incremental catch-up and require a successful validation report.
4. Start a MySQL/Redis instance on a green port and verify `/api/health`, panel
   read endpoints, `/v1/models`, and one test-key inference.
5. Switch Nginx to the green port, drain the old workers, then enable the normal
   pricing, token-refresh, and account-health schedulers.

## Rollback

Keep the final SQLite database and its WAL files until MySQL has passed a full
backup cycle. To roll back, remove the MySQL/Redis environment file from the
service, restore the previous binary, and point Nginx back to the SQLite
instance. Never merge billing by replacing MySQL with an older SQLite snapshot.

Install `ccmax-mysql-backup.sh` and the provided systemd service/timer after
cutover. A backup is accepted only after `gzip -t` succeeds. Keep the final
SQLite snapshot until at least one MySQL backup has also been restored into a
temporary database and its core row counts have been checked.
