#!/usr/bin/env bash
set -euo pipefail

umask 077

backup_dir="${CCMAX_MYSQL_BACKUP_DIR:-/var/lib/ccmax-manager/mysql-backups}"
client_config="${CCMAX_MYSQL_CLIENT_CONFIG:-/etc/ccmax-manager-secrets/mysql-client.cnf}"
retention_days="${CCMAX_MYSQL_BACKUP_RETENTION_DAYS:-14}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
target="$backup_dir/ccmax-$stamp.sql.gz"
temporary="$target.partial"

test -r "$client_config"
install -d -m 0700 "$backup_dir"

cleanup() {
	if test -e "$temporary"; then
		unlink "$temporary"
	fi
}
trap cleanup EXIT INT TERM

mysqldump \
	--defaults-extra-file="$client_config" \
	--single-transaction \
	--quick \
	--routines \
	--events \
	--triggers \
	--hex-blob \
	--no-tablespaces \
	--set-gtid-purged=OFF \
	--databases ccmax | gzip -1 >"$temporary"

gzip -t "$temporary"
mv "$temporary" "$target"
find "$backup_dir" -type f -name 'ccmax-*.sql.gz' -mtime "+$retention_days" -delete

printf 'backup=%s bytes=%s\n' "$target" "$(stat -c %s "$target")"
