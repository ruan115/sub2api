package testcluster

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRejectRoutingEnvironmentWithoutEcho(t *testing.T) {
	for _, name := range []string{"PGHOST", "PGHOSTADDR", "PGSERVICE", "PGPASSWORD", "PGOPTIONS", "PGUNKNOWN", "DATABASE_URL", "DATABASE_DSN"} {
		if err := checkEnvironment([]string{name + "=synthetic-secret"}); !errors.Is(err, errIsolation) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("routing variable not safely rejected: %s", name)
		}
	}
	if err := checkEnvironment([]string{"PORTUNEX_TEST_PG_BIN=/explicit/bin", "PATH=/usr/bin"}); err != nil {
		t.Fatal(err)
	}
	for _, value := range processEnvironment() {
		if strings.HasPrefix(value, "PG") || strings.HasPrefix(value, "HOME=") || strings.HasPrefix(value, "LD_") || strings.HasPrefix(value, "DYLD_") {
			t.Fatal("unsafe process environment")
		}
	}
}

func TestBinaryDirectoryRejectsNonExplicitAndLinks(t *testing.T) {
	for _, path := range []string{"", ".", "/", "../bin", "/missing-portunex-binary-dir"} {
		if binaryDirectory(path) == nil {
			t.Fatal("unusable binary directory accepted")
		}
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"initdb", "postgres"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("synthetic nonexecuted test file"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := binaryDirectory(root); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if binaryDirectory(link) == nil {
		t.Fatal("symlinked binary directory accepted")
	}
	if err := os.Chmod(filepath.Join(root, "initdb"), 0600); err != nil {
		t.Fatal(err)
	}
	if binaryDirectory(root) == nil {
		t.Fatal("non-executable accepted")
	}
}

func TestConnectionOptionsAreExplicitAndEscaped(t *testing.T) {
	dsn := connectionString("/private/a'b\\c", "portunex_test_x")
	for _, required := range []string{"port=5432", "user=portunex_test_admin", "password=synthetic-unused", "sslmode=disable", "search_path=pg_catalog", `host='/private/a\'b\\c'`} {
		if !strings.Contains(dsn, required) {
			t.Fatalf("missing fixed option: %s", required)
		}
	}
}

func TestSocketPathRejectsListSyntaxBeforeServerStartup(t *testing.T) {
	for _, path := range []string{"/tmp/a,/tmp/b", "/tmp/space dir", `/tmp/"quoted"`, "/tmp/line\nbreak", "/tmp/单元", "/tmp/a\\b", "relative", "/tmp/../tmp/s", "/" + strings.Repeat("x", 100)} {
		if socketPath(path) == nil {
			t.Fatal("unsafe socket path accepted")
		}
	}
	if err := socketPath("/private/var/tmp/ptx-pg-123/s"); err != nil {
		t.Fatal(err)
	}
}
