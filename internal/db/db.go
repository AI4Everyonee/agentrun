package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AI4Everyonee/agentrun/internal/schema"
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

// Migrate executes the embedded schema.sql against db, then applies any
// pending migrations from migrations.go in order.
//
// The base schema.sql is idempotent (every CREATE uses IF NOT EXISTS), so
// running Migrate against a fresh or already-initialized DB is safe. The
// migration sequence is tracked in the schema_versions table; each migration
// runs at most once per DB.
//
// Some PRAGMAs in schema.sql are redundant with the DSN — that is fine.
func Migrate(db *sql.DB) error {
	if _, err := db.Exec(schema.SQL); err != nil {
		return fmt.Errorf("db.Migrate: exec schema: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		return fmt.Errorf("db.Migrate: apply migrations: %w", err)
	}
	return nil
}

// CurrentSchemaVersion returns the highest migration version recorded in the
// database. Returns 0 if the schema_versions table does not exist yet or has
// no rows. Used by `agentrun doctor` for sanity checks.
func CurrentSchemaVersion(db *sql.DB) (int, error) {
	var v int
	row := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_versions`)
	if err := row.Scan(&v); err != nil {
		// schema_versions itself may not exist on very-old DBs; treat as 0.
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return v, nil
}

// LatestSchemaVersion returns the highest version known to this binary —
// i.e., the version a fresh DB would reach after Migrate.
func LatestSchemaVersion() int {
	max := 0
	for _, m := range migrations {
		if m.Version > max {
			max = m.Version
		}
	}
	return max
}

// OpenReadWrite opens (but does NOT migrate) an existing SQLite database.
// Intended for short-lived processes — like `agentrun hook` — that run many
// times per session and must avoid the cost of re-executing schema.sql on
// every invocation.
//
// The caller is responsible for guaranteeing the DB exists and the schema is
// applied (this is true for any DB opened previously by db.Open). If the file
// is missing or the schema is absent, subsequent queries will fail loudly.
func OpenReadWrite(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("db.OpenReadWrite: sql.Open: %w", err)
	}
	// Single connection is fine; the hook process is short-lived.
	d.SetMaxOpenConns(1)
	return d, nil
}
