// Package testcluster contains guards for explicitly opted-in synthetic PG tests.
// Cluster creation is compiled only with the portunex_integration build tag.
package testcluster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var errIsolation = errors.New("isolated PostgreSQL test configuration rejected")

// pq parses the process environment before explicit connection options, and
// some PG variables cause a panic. Reject rather than globally mutating env.
func checkEnvironment(environment []string) error {
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "PG") || name == "DATABASE_URL" || name == "DATABASE_DSN" {
			return errIsolation
		}
	}
	return nil
}

func binaryDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return errIsolation
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return errIsolation
	}
	for _, name := range []string{"initdb", "postgres"} {
		info, err := os.Lstat(filepath.Join(path, name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return errIsolation
		}
	}
	return nil
}

func quote(value string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(value) + "'"
}

func connectionString(socket, database string) string {
	return "host=" + quote(socket) + " port=5432 dbname=" + quote(database) +
		" user=portunex_test_admin password=synthetic-unused sslmode=disable connect_timeout=2" +
		" application_name=portunex_recovery_test client_encoding=UTF8 timezone=UTC" +
		" options='-c search_path=pg_catalog -c statement_timeout=5000 -c lock_timeout=2000'"
}

func processEnvironment() []string {
	return []string{"PATH=/usr/bin:/bin", "LC_ALL=C", "LANG=C", "TZ=UTC"}
}

// unix_socket_directories is a PostgreSQL list-valued GUC, not an opaque path.
// Reject paths needing quoting rather than allowing inherited TMPDIR to add a
// second server socket. This deliberately supports only a narrow temp path set.
func socketPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(filepath.Join(path, ".s.PGSQL.5432")) >= 104 {
		return errIsolation
	}
	for _, c := range path {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '/' || c == '-' || c == '_' || c == '.') {
			return errIsolation
		}
	}
	return nil
}
