// Package db wraps the Postgres connection pool and exposes the few queries
// the watcher needs. Everything is plain pgx — no ORM, no abstractions.
package db

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AI4Everyonee/agentrun/internal/parse"
)

//go:embed schema.sql
var schemaFS embed.FS

// Open dials Postgres with the given DSN and applies schema.sql idempotently.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("db.Open: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.Open: ping: %w", err)
	}

	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.Open: read embedded schema: %w", err)
	}
	if _, err := pool.Exec(ctx, string(schema)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db.Open: apply schema: %w", err)
	}
	return pool, nil
}

// UpsertSession either inserts a new session row or updates the mutable fields
// (model, ended_at, user_name, cwd) on an existing one. ON CONFLICT semantics
// keep the earliest started_at and the latest ended_at.
func UpsertSession(ctx context.Context, pool *pgxpool.Pool, s parse.Session) error {
	const q = `
INSERT INTO sessions (
    agent, session_uuid, user_name, cwd, model,
    started_at, ended_at, transcript_path
) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''),
          $6, $7, $8)
ON CONFLICT (agent, session_uuid) DO UPDATE SET
    user_name       = COALESCE(sessions.user_name, EXCLUDED.user_name),
    cwd             = COALESCE(sessions.cwd, EXCLUDED.cwd),
    model           = COALESCE(sessions.model, EXCLUDED.model),
    started_at      = LEAST(sessions.started_at, EXCLUDED.started_at),
    ended_at        = GREATEST(sessions.ended_at, EXCLUDED.ended_at),
    transcript_path = EXCLUDED.transcript_path
`
	var endedAt any
	if !s.EndedAt.IsZero() {
		endedAt = s.EndedAt
	}
	_, err := pool.Exec(ctx, q,
		string(s.Agent), s.UUID, s.UserName, s.Cwd, s.Model,
		s.StartedAt, endedAt, s.TranscriptPath,
	)
	if err != nil {
		return fmt.Errorf("UpsertSession: %w", err)
	}
	return nil
}

// InsertEventsAndCheckpoint inserts a batch of events and updates ingest_state
// for the source file — all in one transaction. Either everything lands or
// nothing does, so a crash leaves us consistent.
//
// Events with role=system and empty content are dropped to keep the table from
// being dominated by metadata noise; we still advance the offset past them.
func InsertEventsAndCheckpoint(
	ctx context.Context,
	pool *pgxpool.Pool,
	agent parse.Agent,
	sessionUUID string,
	events []parse.Event,
	filePath string,
	inode int64,
	byteOffset int64,
) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("InsertEventsAndCheckpoint: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck — no-op once Commit succeeds

	if len(events) > 0 {
		// ON CONFLICT DO NOTHING means a re-run after partial failure is safe.
		const q = `
INSERT INTO events (agent, session_uuid, seq, ts, role, tool_name, content, payload)
VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), $8)
ON CONFLICT (agent, session_uuid, seq) DO NOTHING
`
		for _, e := range events {
			payload := sanitizeJSONBytes(e.Payload)
			if _, err := tx.Exec(ctx, q,
				string(agent), sessionUUID, e.Seq, e.Ts,
				string(e.Role), e.ToolName,
				sanitizeText(e.Content), payload,
			); err != nil {
				return fmt.Errorf("InsertEventsAndCheckpoint: exec event seq=%d (payload %d bytes): %w",
					e.Seq, len(payload), err)
			}
		}
	}

	const upsertState = `
INSERT INTO ingest_state (file_path, inode, byte_offset)
VALUES ($1, $2, $3)
ON CONFLICT (file_path) DO UPDATE SET
    inode       = EXCLUDED.inode,
    byte_offset = EXCLUDED.byte_offset,
    updated_at  = now()
`
	if _, err := tx.Exec(ctx, upsertState, filePath, inode, byteOffset); err != nil {
		return fmt.Errorf("InsertEventsAndCheckpoint: upsert state: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("InsertEventsAndCheckpoint: commit: %w", err)
	}
	return nil
}

// sanitizeJSONBytes strips characters that Postgres's JSONB parser rejects:
//
//   - raw 0x00 bytes anywhere in the payload (real Codex transcripts include
//     these when Bash output contains nulls)
//   - the `\u0000` escape sequence in JSON string values (Postgres considers
//     this an unsupported Unicode escape because PG TEXT can't store NUL)
//
// We replace both with safe substitutes rather than dropping the byte so the
// payload's length and structure stay roughly intact for forensic reads.
func sanitizeJSONBytes(b []byte) []byte {
	if !bytes.ContainsAny(b, "\x00") && !bytes.Contains(b, []byte(`\u0000`)) {
		return b
	}
	out := bytes.ReplaceAll(b, []byte{0x00}, []byte{' '})
	out = bytes.ReplaceAll(out, []byte(`\u0000`), []byte(`\u0020`))
	return out
}

// sanitizeText strips raw 0x00 bytes from a text column value. Postgres's TEXT
// type doesn't allow NUL — even though our JSONB sanitizer handles the JSON
// envelope, the extracted content field is a plain string and needs its own
// pass.
func sanitizeText(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// LoadIngestState reads the recorded (inode, byteOffset) for a file.
// Returns (0, 0, false) if the file isn't tracked yet — caller should start
// from the beginning.
func LoadIngestState(ctx context.Context, pool *pgxpool.Pool, filePath string) (inode, offset int64, ok bool, err error) {
	row := pool.QueryRow(ctx,
		`SELECT inode, byte_offset FROM ingest_state WHERE file_path = $1`,
		filePath,
	)
	err = row.Scan(&inode, &offset)
	if err == pgx.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("LoadIngestState: %w", err)
	}
	return inode, offset, true, nil
}
