package store

import (
	"crypto/sha256"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Migration struct {
	Name     string
	SQL      string
	Checksum [32]byte
}

func Migrations(direction string) ([]Migration, error) {
	if direction != "up" && direction != "down" {
		return nil, fmt.Errorf("unsupported migration direction %q", direction)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	suffix := "." + direction + ".sql"
	var migrations []Migration
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), suffix) {
			continue
		}
		payload, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, Migration{Name: entry.Name(), SQL: string(payload), Checksum: sha256.Sum256(payload)})
	}
	sort.Slice(migrations, func(left, right int) bool { return migrations[left].Name < migrations[right].Name })
	return migrations, nil
}
