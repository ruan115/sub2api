//go:build !sqlite_migrate

package main

import (
	"log"
	"os"
	"strings"
)

func main() {
	if strings.TrimSpace(os.Getenv("CCMAX_MYSQL_DSN")) == "" {
		log.Fatal("CCMAX_MYSQL_DSN is required; the production server no longer falls back to SQLite")
	}
	runServer()
}
