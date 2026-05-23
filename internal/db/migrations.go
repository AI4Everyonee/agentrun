package db

import (
	"database/sql"
	"fmt"
)

// migration is a single named change applied exactly once per database.
//
// Conventions:
//   - Version is monotonically increasing; never reorder existing entries.
//   - SQL is one or more semicolon-separated statements run inside a transaction.
//   - Migrations MUST be additive (ALTER TABLE … ADD COLUMN, CREATE TABLE/INDEX,
//     CREATE VIRTUAL TABLE). Never drop or rename columns — old rows live on.
//   - Bumping the embedded schema.sql is for FRESH installs only; existing DBs
//     reach the same shape via the migration sequence.
type migration struct {
	Version int
	Name    string
	SQL     string
}

// migrations is the ordered list of changes applied on top of the base
// schema.sql. Append-only. Tests assert ordering stays monotonic.
var migrations = []migration{
	{
		Version: 1,
		Name:    "add_user_name_to_sessions",
		SQL:     `ALTER TABLE sessions ADD COLUMN user_name TEXT;`,
	},
	{
		Version: 2,
		Name:    "add_tokens_and_cost_to_sessions",
		SQL: `ALTER TABLE sessions ADD COLUMN tokens_used INTEGER;
		      ALTER TABLE sessions ADD COLUMN cost_usd_cents INTEGER;`,
	},
	{
		Version: 3,
		Name:    "add_tags_to_events",
		SQL:     `ALTER TABLE events ADD COLUMN tags TEXT;`,
	},
	{
		Version: 4,
		Name:    "create_events_fts5",
		// FTS5 mirror of events.payload_json so `agentrun search` is fast. The
		// trigger keeps it in sync on inserts; we never UPDATE events so a
		// matching update trigger isn't needed.
		SQL: `CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
		         id UNINDEXED,
		         session_id UNINDEXED,
		         type UNINDEXED,
		         payload,
		         content='events',
		         content_rowid='rowid'
		      );
		      CREATE TRIGGER IF NOT EXISTS events_ai_fts AFTER INSERT ON events BEGIN
		         INSERT INTO events_fts(rowid, id, session_id, type, payload)
		         VALUES (new.rowid, new.id, new.session_id, new.type, new.payload_json);
		      END;`,
	},
}

// ensureVersionTable creates schema_versions if it does not already exist.
// Called by Migrate as the very first step before any version-gated change.
func ensureVersionTable(d *sql.DB) error {
	_, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS schema_versions (
		    version    INTEGER PRIMARY KEY,
		    name       TEXT NOT NULL,
		    applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		);
	`)
	if err != nil {
		return fmt.Errorf("ensure schema_versions: %w", err)
	}
	return nil
}

// appliedVersions returns the set of migration versions already recorded.
func appliedVersions(d *sql.DB) (map[int]bool, error) {
	rows, err := d.Query(`SELECT version FROM schema_versions`)
	if err != nil {
		return nil, fmt.Errorf("read schema_versions: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// applyMigrations runs each pending migration in a transaction, recording it
// in schema_versions on success. A failure aborts the whole transaction and
// leaves schema_versions consistent.
//
// Safe to call on a DB that already has the base schema.sql applied — the
// migrations table tracks state independently of CREATE TABLE IF NOT EXISTS.
func applyMigrations(d *sql.DB) error {
	if err := ensureVersionTable(d); err != nil {
		return err
	}
	applied, err := appliedVersions(d)
	if err != nil {
		return err
	}
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if err := applyOne(d, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

func applyOne(d *sql.DB, m migration) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.Exec(m.SQL); err != nil {
		return fmt.Errorf("exec sql: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_versions (version, name) VALUES (?, ?)`, m.Version, m.Name); err != nil {
		return fmt.Errorf("record version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	committed = true
	return nil
}
