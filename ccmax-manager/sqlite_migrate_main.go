//go:build sqlite_migrate

package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
)

func main() {
	source := strings.TrimSpace(os.Getenv("CCMAX_MIGRATE_FROM_SQLITE"))
	if source == "" {
		log.Fatal("CCMAX_MIGRATE_FROM_SQLITE is required")
	}
	if strings.TrimSpace(os.Getenv("CCMAX_MYSQL_DSN")) == "" {
		log.Fatal("CCMAX_MYSQL_DSN is required")
	}
	a, err := newApp("")
	if err != nil {
		log.Fatal(err)
	}
	defer a.db.Close()
	defer a.redis.Close()
	report, err := migrateSQLiteToMySQL(
		source,
		a.db,
		strings.TrimSpace(os.Getenv("CCMAX_MIGRATE_RESET_TARGET")) == "1",
		strings.TrimSpace(os.Getenv("CCMAX_MIGRATE_INCREMENTAL")) == "1",
	)
	if err != nil {
		log.Fatal(err)
	}
	encoded, _ := json.Marshal(report)
	log.Printf("SQLite to MySQL migration completed: %s", encoded)
}
