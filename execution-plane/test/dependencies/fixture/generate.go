package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

const (
	runtimeDatabase = "isthmus_p5_runtime"
	ccmaxDatabase   = "isthmus_p5_ccmax"
	runtimeUser     = "isthmus_runtime_test"
	ccmaxUser       = "isthmus_ccmax_test"
)

func randomPassword() (string, error) {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate test credential: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func generate(cfg config) error {
	if cfg.output == "" || cfg.ccmaxSource == "" {
		return fmt.Errorf("--output and --ccmax-schema-source are required")
	}
	if cfg.mysqlPort < 1024 || cfg.mysqlPort > 65535 || cfg.redisPort < 1024 || cfg.redisPort > 65535 || cfg.mysqlPort == cfg.redisPort {
		return fmt.Errorf("test ports must be distinct unprivileged TCP ports")
	}
	source, err := os.ReadFile(cfg.ccmaxSource)
	if err != nil {
		return fmt.Errorf("read CCMAX schema source: %w", err)
	}
	ccmaxSchema, err := extractCCMAXSchema(source)
	if err != nil {
		return err
	}
	migrations, err := store.Migrations("up")
	if err != nil {
		return fmt.Errorf("load embedded runtime migrations: %w", err)
	}
	passwords := make([]string, 4)
	for index := range passwords {
		passwords[index], err = randomPassword()
		if err != nil {
			return err
		}
	}
	rootPassword, runtimePassword, ccmaxPassword, redisPassword := passwords[0], passwords[1], passwords[2], passwords[3]
	var schema strings.Builder
	fmt.Fprintf(&schema, "CREATE DATABASE %s CHARACTER SET utf8mb4 COLLATE utf8mb4_bin;\n", runtimeDatabase)
	fmt.Fprintf(&schema, "CREATE DATABASE %s CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;\n", ccmaxDatabase)
	fmt.Fprintf(&schema, "USE %s;\n", runtimeDatabase)
	for _, migration := range migrations {
		fmt.Fprintf(&schema, "-- %s\n%s\n", migration.Name, migration.SQL)
	}
	fmt.Fprintf(&schema, "USE %s;\n%s\n", ccmaxDatabase, strings.Join(ccmaxSchema, "\n"))
	for _, account := range []struct{ user, password, database string }{
		{runtimeUser, runtimePassword, runtimeDatabase},
		{ccmaxUser, ccmaxPassword, ccmaxDatabase},
	} {
		fmt.Fprintf(&schema, "CREATE USER '%s'@'%%' IDENTIFIED BY '%s';\n", account.user, account.password)
		fmt.Fprintf(&schema, "GRANT SELECT, INSERT, UPDATE, DELETE ON %s.* TO '%s'@'%%';\n", account.database, account.user)
	}
	files := []struct{ name, content string }{
		{"root-password", rootPassword + "\n"},
		{"admin.cnf", "[client]\nuser=root\npassword=" + rootPassword + "\n"},
		{"redis.conf", "bind 0.0.0.0\nprotected-mode yes\nrequirepass " + redisPassword + "\nsave \"\"\nappendonly no\nmaxmemory 64mb\nmaxmemory-policy noeviction\n"},
		{"init.sql", schema.String()},
		{"test.env", fmt.Sprintf(
			"export EXECUTION_MYSQL_TEST_DSN='%s:%s@tcp(127.0.0.1:%d)/%s?parseTime=true&loc=UTC'\n"+
				"export EXECUTION_CCMAX_MYSQL_TEST_DSN='%s:%s@tcp(127.0.0.1:%d)/%s?parseTime=true&loc=UTC'\n"+
				"export EXECUTION_REDIS_TEST_URL='redis://:%s@127.0.0.1:%d/0'\n",
			runtimeUser, runtimePassword, cfg.mysqlPort, runtimeDatabase,
			ccmaxUser, ccmaxPassword, cfg.mysqlPort, ccmaxDatabase,
			redisPassword, cfg.redisPort,
		)},
	}
	// Mkdir, rather than MkdirAll, fails closed for existing directories and
	// symlinks. Never overwrite credentials or partially initialized state.
	if err := os.Mkdir(cfg.output, 0o700); err != nil {
		return fmt.Errorf("create new private output directory: %w", err)
	}
	for _, file := range files {
		if err := writePrivate(filepath.Join(cfg.output, file.name), file.content); err != nil {
			return fmt.Errorf("write %s (partial private output retained): %w", file.name, err)
		}
	}
	return nil
}

func writePrivate(path, value string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString(value)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}
