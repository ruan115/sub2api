package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func testConfig(t *testing.T) config {
	t.Helper()
	return config{
		output: filepath.Join(t.TempDir(), "private"), ccmaxSource: "../../../../ccmax-manager/execution_outbox.go",
		mysqlPort: 33379, redisPort: 63979,
	}
}

func readFixture(t *testing.T, cfg config, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfg.output, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestGeneratePrivateFreshFixturesAndLeastPrivileges(t *testing.T) {
	cfg := testConfig(t)
	if err := generate(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.output)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private directory permissions: err=%v", err)
	}
	entries, err := os.ReadDir(cfg.output)
	if err != nil || len(entries) != 5 {
		t.Fatalf("generated file count=%d err=%v", len(entries), err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Errorf("nonprivate file %s: err=%v", entry.Name(), err)
		}
	}
	root := strings.TrimSpace(readFixture(t, cfg, "root-password"))
	if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(root) {
		t.Fatal("root password is not 32 random bytes encoded safely")
	}
	if got := readFixture(t, cfg, "admin.cnf"); got != "[client]\nuser=root\npassword="+root+"\n" {
		t.Fatal("admin client does not use generated root credential")
	}
	sql := readFixture(t, cfg, "init.sql")
	for _, marker := range []string{
		"CREATE DATABASE isthmus_p5_runtime", "CREATE DATABASE isthmus_p5_ccmax",
		"001_runtime_core.up.sql", "014_runtime_certificates.up.sql",
		"CREATE TABLE runtime_certificates", "CREATE TABLE IF NOT EXISTS runtime_outbox",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON isthmus_p5_runtime.* TO 'isthmus_runtime_test'@'%'",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON isthmus_p5_ccmax.* TO 'isthmus_ccmax_test'@'%'",
	} {
		if !strings.Contains(sql, marker) {
			t.Errorf("missing schema/privilege marker %s", marker)
		}
	}
	for _, forbidden := range []string{"GRANT ALL", "ON *.*", "GRANT OPTION", "DROP DATABASE", "CREATE TABLE IF NOT EXISTS accounts", root} {
		if strings.Contains(sql, forbidden) {
			t.Error("SQL contains a forbidden privilege, schema or root credential")
		}
	}
	env := readFixture(t, cfg, "test.env")
	if strings.Count(env, "export ") != 3 || strings.Contains(env, root) || strings.Contains(env, "root:") {
		t.Fatal("test environment includes unexpected variables or admin credential")
	}
	for _, marker := range []string{"EXECUTION_MYSQL_TEST_DSN=", "EXECUTION_CCMAX_MYSQL_TEST_DSN=", "EXECUTION_REDIS_TEST_URL=", "@tcp(127.0.0.1:33379)", "@127.0.0.1:63979/0", "?parseTime=true&loc=UTC"} {
		if !strings.Contains(env, marker) {
			t.Errorf("missing environment marker %s", marker)
		}
	}
	redis := readFixture(t, cfg, "redis.conf")
	for _, marker := range []string{"bind 0.0.0.0\n", "protected-mode yes\n", "requirepass ", "save \"\"\n", "appendonly no\n", "maxmemory 64mb\n", "maxmemory-policy noeviction\n"} {
		if !strings.Contains(redis, marker) {
			t.Errorf("missing Redis configuration marker %s", marker)
		}
	}
}

func TestGenerateRejectsExistingDirectoryAndSymlinkWithoutChangingFiles(t *testing.T) {
	cfg := testConfig(t)
	if err := generate(cfg); err != nil {
		t.Fatal(err)
	}
	before := readFixture(t, cfg, "root-password")
	if err := generate(cfg); err == nil {
		t.Fatal("overwrote existing output directory")
	}
	if readFixture(t, cfg, "root-password") != before {
		t.Fatal("changed existing credential")
	}
	cfg.output = t.TempDir()
	if err := generate(cfg); err == nil {
		t.Fatal("accepted existing empty directory")
	}
	target := t.TempDir()
	cfg.output = filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink(target, cfg.output); err != nil {
		t.Fatal(err)
	}
	if err := generate(cfg); err == nil {
		t.Fatal("accepted output symlink")
	}
	entries, err := os.ReadDir(target)
	if err != nil || len(entries) != 0 {
		t.Fatal("output symlink target was modified")
	}
}

func TestIndependentPasswordsAndNoCredentialLogging(t *testing.T) {
	passwordPattern := regexp.MustCompile(`[a-f0-9]{64}`)
	seen := map[string]bool{}
	for range 2 {
		cfg := testConfig(t)
		var stdout, stderr bytes.Buffer
		if err := run([]string{"--output", cfg.output, "--ccmax-schema-source", cfg.ccmaxSource}, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		var credentials []string
		credentials = append(credentials, strings.TrimSpace(readFixture(t, cfg, "root-password")))
		credentials = append(credentials, passwordPattern.FindAllString(readFixture(t, cfg, "test.env"), -1)...)
		if len(credentials) != 4 {
			t.Fatal("expected four independent credentials")
		}
		for _, credential := range credentials {
			if seen[credential] {
				t.Fatal("credential reused across purposes or invocations")
			}
			seen[credential] = true
			if strings.Contains(stdout.String(), credential) || strings.Contains(stderr.String(), credential) {
				t.Fatal("credential was logged")
			}
		}
	}
}

func TestInvalidConfigurationDoesNotCreateOutput(t *testing.T) {
	for _, mutate := range []func(*config){
		func(c *config) { c.mysqlPort = 0 },
		func(c *config) { c.redisPort = 65536 },
		func(c *config) { c.redisPort = c.mysqlPort },
		func(c *config) { c.ccmaxSource = "" },
	} {
		cfg := testConfig(t)
		mutate(&cfg)
		if err := generate(cfg); err == nil {
			t.Error("accepted invalid configuration")
		}
		if _, err := os.Stat(cfg.output); !os.IsNotExist(err) {
			t.Fatal("created output for invalid configuration")
		}
	}
}
