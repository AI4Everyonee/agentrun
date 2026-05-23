package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jeevan/agentrun/internal/schema"
	_ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite database at path and applies the schema.
// It also creates the parent directory if missing. WAL mode is set via PRAGMAs.
//
// DSN format:
//
//	file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)
//
// The driver name "sqlite" is registered by modernc.org/sqlite.
func Open(path string) (*sql.DB, error) {
	// Create the parent directory if it does not exist.
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("db.Open: create parent dir %q: %w", dir, err)
	}

	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db.Open: sql.Open: %w", err)
	}

	if err := Migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Open: migrate: %w", err)
	}

	return db, nil
}

// Migrate executes the embedded schema.sql against db. Idempotent because every
// CREATE TABLE/INDEX statement in schema.sql uses IF NOT EXISTS.
// Some PRAGMAs in schema.sql are redundant with the DSN — that is fine; PRAGMA
// execution is idempotent.
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(schema.SQL); err != nil {
		return fmt.Errorf("db.Migrate: exec schema: %w", err)
	}
	return nil
}
