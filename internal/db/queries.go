package db

import (
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/jeevan/agentrun/internal/ids"
)

// SessionRow maps to the sessions table.
// Go field -> SQL column:
//
//	ID             -> id
//	Agent          -> agent
//	AgentVersion   -> agent_version
//	Model          -> model
//	PermissionMode -> permission_mode
//	Cwd            -> cwd
//	RepoRoot       -> repo_root
//	Branch         -> branch
//	StartCommitSHA -> start_commit_sha
//	EndCommitSHA   -> end_commit_sha
//	StartedAt      -> started_at
//	EndedAt        -> ended_at
//	ExitCode       -> exit_code
//	TranscriptPath -> transcript_path
//	PID            -> pid
//	MetadataJSON   -> metadata_json
type SessionRow struct {
	ID             string
	Agent          string
	AgentVersion   sql.NullString
	Model          sql.NullString
	PermissionMode sql.NullString
	Cwd            string
	RepoRoot       sql.NullString
	Branch         sql.NullString
	StartCommitSHA sql.NullString
	EndCommitSHA   sql.NullString
	StartedAt      time.Time
	EndedAt        sql.NullTime
	ExitCode       sql.NullInt64
	TranscriptPath sql.NullString
	PID            sql.NullInt64
	MetadataJSON   string
}

// EventRow maps to the events table.
// Go field -> SQL column:
//
//	ID               -> id
//	SessionID        -> session_id
//	Sequence         -> sequence
//	Ts               -> ts
//	Source           -> source
//	Type             -> type
//	PayloadJSON      -> payload_json
//	RedactionVersion -> redaction_version
type EventRow struct {
	ID               string
	SessionID        string
	Sequence         int64
	Ts               time.Time
	Source           string
	Type             string
	PayloadJSON      []byte
	RedactionVersion sql.NullString
}

// ArtifactRow maps to the artifacts table.
// Go field -> SQL column:
//
//	ID           -> id
//	SessionID    -> session_id
//	EventID      -> event_id
//	Kind         -> kind
//	Path         -> path
//	ContentHash  -> content_hash
//	SizeBytes    -> size_bytes
//	Mime         -> mime
//	MetadataJSON -> metadata_json
type ArtifactRow struct {
	ID           string
	SessionID    string
	EventID      sql.NullString
	Kind         string
	Path         sql.NullString
	ContentHash  sql.NullString
	SizeBytes    sql.NullInt64
	Mime         sql.NullString
	MetadataJSON string
}

// SessionListRow is the joined sessions + session_summary row used by `agentrun sessions`.
type SessionListRow struct {
	ID        string
	Agent     string
	RepoRoot  sql.NullString
	Cwd       string
	StartedAt time.Time
	Status    sql.NullString
}

// fmtTime formats t as RFC3339Nano UTC for SQLite TEXT columns.
func fmtTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseTime parses a RFC3339Nano UTC string from a SQLite TEXT column.
func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parseTime %q: %w", s, err)
	}
	return t, nil
}

// InsertSession inserts a new row into sessions.
func InsertSession(db *sql.DB, s SessionRow) error {
	const q = `INSERT INTO sessions (
		id, agent, agent_version, model, permission_mode,
		cwd, repo_root, branch, start_commit_sha, end_commit_sha,
		started_at, ended_at, exit_code, transcript_path, pid, metadata_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var endedAt interface{}
	if s.EndedAt.Valid {
		endedAt = fmtTime(s.EndedAt.Time)
	}

	_, err := db.Exec(q,
		s.ID, s.Agent, s.AgentVersion, s.Model, s.PermissionMode,
		s.Cwd, s.RepoRoot, s.Branch, s.StartCommitSHA, s.EndCommitSHA,
		fmtTime(s.StartedAt), endedAt, s.ExitCode, s.TranscriptPath, s.PID,
		s.MetadataJSON,
	)
	if err != nil {
		return fmt.Errorf("InsertSession: %w", err)
	}
	return nil
}

// UpdateSessionPID updates the pid column for a session.
func UpdateSessionPID(db *sql.DB, sessionID string, pid int) error {
	_, err := db.Exec(`UPDATE sessions SET pid=? WHERE id=?`, pid, sessionID)
	if err != nil {
		return fmt.Errorf("UpdateSessionPID: %w", err)
	}
	return nil
}

// FinalizeSession atomically updates sessions.ended_at/exit_code/end_commit_sha and
// session_summary.status within a single transaction.
// COALESCE(NULLIF(?, ''), end_commit_sha) preserves the existing SHA when endCommitSHA is "".
func FinalizeSession(db *sql.DB, sessionID, endCommitSHA, status string, exitCode int, endedAt time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("FinalizeSession: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	_, err = tx.Exec(
		`UPDATE sessions SET ended_at=?, exit_code=?, end_commit_sha=COALESCE(NULLIF(?, ''), end_commit_sha) WHERE id=?`,
		fmtTime(endedAt), exitCode, endCommitSHA, sessionID,
	)
	if err != nil {
		return fmt.Errorf("FinalizeSession: update sessions: %w", err)
	}

	_, err = tx.Exec(
		`UPDATE session_summary SET status=? WHERE session_id=?`,
		status, sessionID,
	)
	if err != nil {
		return fmt.Errorf("FinalizeSession: update session_summary: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("FinalizeSession: commit: %w", err)
	}
	return nil
}

// ListSessions returns up to limit sessions ordered by started_at DESC, LEFT JOINed
// with session_summary so sessions without a summary row still appear (defensive).
func ListSessions(db *sql.DB, limit int) ([]SessionListRow, error) {
	const q = `
		SELECT s.id, s.agent, s.repo_root, s.cwd, s.started_at, ss.status
		FROM sessions s
		LEFT JOIN session_summary ss ON ss.session_id = s.id
		ORDER BY s.started_at DESC
		LIMIT ?`

	rows, err := db.Query(q, limit)
	if err != nil {
		return nil, fmt.Errorf("ListSessions: query: %w", err)
	}
	defer rows.Close()

	var result []SessionListRow
	for rows.Next() {
		var r SessionListRow
		var startedAt string
		if err := rows.Scan(&r.ID, &r.Agent, &r.RepoRoot, &r.Cwd, &startedAt, &r.Status); err != nil {
			return nil, fmt.Errorf("ListSessions: scan: %w", err)
		}
		r.StartedAt, err = parseTime(startedAt)
		if err != nil {
			return nil, fmt.Errorf("ListSessions: parse started_at: %w", err)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListSessions: rows: %w", err)
	}
	return result, nil
}

// GetSession retrieves a single session by ID.
// Returns sql.ErrNoRows directly when the session does not exist.
func GetSession(db *sql.DB, sessionID string) (SessionRow, error) {
	const q = `SELECT
		id, agent, agent_version, model, permission_mode,
		cwd, repo_root, branch, start_commit_sha, end_commit_sha,
		started_at, ended_at, exit_code, transcript_path, pid, metadata_json
	FROM sessions WHERE id=?`

	row := db.QueryRow(q, sessionID)

	var s SessionRow
	var startedAt string
	var endedAt sql.NullString

	err := row.Scan(
		&s.ID, &s.Agent, &s.AgentVersion, &s.Model, &s.PermissionMode,
		&s.Cwd, &s.RepoRoot, &s.Branch, &s.StartCommitSHA, &s.EndCommitSHA,
		&startedAt, &endedAt, &s.ExitCode, &s.TranscriptPath, &s.PID,
		&s.MetadataJSON,
	)
	if err != nil {
		// Return sql.ErrNoRows directly so callers can distinguish not-found.
		return SessionRow{}, err
	}

	s.StartedAt, err = parseTime(startedAt)
	if err != nil {
		return SessionRow{}, fmt.Errorf("GetSession: parse started_at: %w", err)
	}

	if endedAt.Valid {
		t, err := parseTime(endedAt.String)
		if err != nil {
			return SessionRow{}, fmt.Errorf("GetSession: parse ended_at: %w", err)
		}
		s.EndedAt = sql.NullTime{Time: t, Valid: true}
	}

	return s, nil
}

// InsertEventStmt returns a prepared INSERT statement for batch use within a transaction.
// The caller is responsible for closing the statement.
func InsertEventStmt(tx *sql.Tx) (*sql.Stmt, error) {
	const q = `INSERT INTO events (id, session_id, sequence, ts, source, type, payload_json, redaction_version)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	stmt, err := tx.Prepare(q)
	if err != nil {
		return nil, fmt.Errorf("InsertEventStmt: prepare: %w", err)
	}
	return stmt, nil
}

// InsertEventOne inserts a single event using its own transaction.
// Used for synchronous emits such as session.started / session.ended.
func InsertEventOne(db *sql.DB, e EventRow) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("InsertEventOne: begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := InsertEventStmt(tx)
	if err != nil {
		return err
	}
	defer stmt.Close()

	_, err = stmt.Exec(
		e.ID, e.SessionID, e.Sequence,
		fmtTime(e.Ts),
		e.Source, e.Type, e.PayloadJSON, e.RedactionVersion,
	)
	if err != nil {
		return fmt.Errorf("InsertEventOne: exec: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("InsertEventOne: commit: %w", err)
	}
	return nil
}

// insertRetryBackoffs is the jittered backoff schedule for SQLITE_BUSY and
// UNIQUE-collision retries. After all retries are exhausted the caller is
// expected to log and continue — never crash the parent agent.
var insertRetryBackoffs = []time.Duration{
	10 * time.Millisecond,
	50 * time.Millisecond,
	200 * time.Millisecond,
}

// isRetryableInsertErr reports whether err is one of the recoverable
// concurrency errors we expect under load.
func isRetryableInsertErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: events.session_id, events.sequence")
}

// sleepJittered sleeps base ±25%. base must be positive.
func sleepJittered(base time.Duration) {
	half := int64(base) / 2
	if half <= 0 {
		time.Sleep(base)
		return
	}
	delta := time.Duration(rand.Int63n(half))
	sign := time.Duration(1)
	if rand.Intn(2) == 0 {
		sign = -1
	}
	d := base + sign*(delta-base/4)
	if d < 0 {
		d = base
	}
	time.Sleep(d)
}

// execWithRetry runs Exec with the standard retry envelope.
func execWithRetry(d *sql.DB, q string, args ...interface{}) error {
	var lastErr error
	for attempt := 0; attempt <= len(insertRetryBackoffs); attempt++ {
		_, err := d.Exec(q, args...)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableInsertErr(err) {
			return err
		}
		if attempt == len(insertRetryBackoffs) {
			break
		}
		sleepJittered(insertRetryBackoffs[attempt])
	}
	return fmt.Errorf("execWithRetry: exhausted retries: %w", lastErr)
}

// InsertEventWithAutoSeq inserts one events row computing sequence as
// MAX(sequence)+1 inside a transaction. Designed for cross-process callers
// (hook subprocesses) that cannot share an in-memory atomic counter.
//
// On SQLITE_BUSY or UNIQUE collisions with concurrent writers, retries up to
// 3 times with jittered backoff (10/50/200ms). Returns the generated event ID
// on success.
//
// All errors after exhausted retries are returned to the caller; the caller
// is expected to log and continue (never block the parent agent).
func InsertEventWithAutoSeq(
	d *sql.DB,
	sessionID string,
	ts time.Time,
	source, eventType string,
	payload []byte,
	redactorVersion string,
) (string, error) {
	var lastErr error
	for attempt := 0; attempt <= len(insertRetryBackoffs); attempt++ {
		evtID, err := tryInsertEventWithAutoSeq(d, sessionID, ts, source, eventType, payload, redactorVersion)
		if err == nil {
			return evtID, nil
		}
		lastErr = err
		if !isRetryableInsertErr(err) {
			return "", err
		}
		if attempt == len(insertRetryBackoffs) {
			break
		}
		sleepJittered(insertRetryBackoffs[attempt])
	}
	return "", fmt.Errorf("InsertEventWithAutoSeq: exhausted retries: %w", lastErr)
}

func tryInsertEventWithAutoSeq(
	d *sql.DB,
	sessionID string,
	ts time.Time,
	source, eventType string,
	payload []byte,
	redactorVersion string,
) (string, error) {
	// d.Begin() issues "BEGIN IMMEDIATE" when the connection was opened with
	// _txlock=immediate (OpenReadWrite does this). BEGIN IMMEDIATE acquires the
	// write lock upfront, preventing SQLITE_BUSY_SNAPSHOT (error 517) that would
	// occur if a deferred transaction upgraded from read to write after seeing a
	// stale snapshot. busy_timeout handles SQLITE_BUSY but NOT SQLITE_BUSY_SNAPSHOT.
	tx, err := d.Begin()
	if err != nil {
		return "", fmt.Errorf("tryInsertEventWithAutoSeq: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var nextSeq int64
	row := tx.QueryRow(
		`SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE session_id = ?`,
		sessionID,
	)
	if err := row.Scan(&nextSeq); err != nil {
		return "", fmt.Errorf("tryInsertEventWithAutoSeq: scan max(seq): %w", err)
	}

	evtID := ids.Event()
	_, err = tx.Exec(
		`INSERT INTO events (id, session_id, sequence, ts, source, type, payload_json, redaction_version)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		evtID, sessionID, nextSeq,
		ts.UTC().Format(time.RFC3339Nano),
		source, eventType, payload,
		sql.NullString{String: redactorVersion, Valid: redactorVersion != ""},
	)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("tryInsertEventWithAutoSeq: commit: %w", err)
	}
	committed = true
	return evtID, nil
}

// allowedSummaryCounters lists session_summary columns safe to increment.
// We hard-code this allowlist to prevent SQL injection via interpolated
// column names.
var allowedSummaryCounters = map[string]bool{
	"user_prompts":      true,
	"tool_calls":        true,
	"errors":            true,
	"approvals_request": true,
	"approvals_denied":  true,
	"files_changed":     true, // reserved for Phase 4
	"commands_run":      true, // reserved for Phase 5
	"validations_run":   true, // reserved for Phase 5
	"validations_pass":  true, // reserved for Phase 5
	"validations_fail":  true, // reserved for Phase 5
}

// IncrementSummaryCounter atomically increments one counter column on the
// session_summary row for sessionID. Same retry envelope as InsertEventWithAutoSeq.
func IncrementSummaryCounter(d *sql.DB, sessionID, counter string) error {
	if !allowedSummaryCounters[counter] {
		return fmt.Errorf("IncrementSummaryCounter: counter %q not in allowlist", counter)
	}
	q := fmt.Sprintf(
		`UPDATE session_summary SET %s = %s + 1 WHERE session_id = ?`,
		counter, counter,
	)
	return execWithRetry(d, q, sessionID)
}

// UpdateSessionModel sets sessions.model. Idempotent (overwrites). No-op on
// empty model. Same retry envelope.
func UpdateSessionModel(d *sql.DB, sessionID, model string) error {
	if model == "" {
		return nil
	}
	return execWithRetry(d, `UPDATE sessions SET model = ? WHERE id = ?`, model, sessionID)
}

// UpdateSessionTranscriptPath sets sessions.transcript_path ONLY IF the
// current value is NULL (so the first hook to report wins; later hooks
// silently no-op). Same retry envelope.
func UpdateSessionTranscriptPath(d *sql.DB, sessionID, path string) error {
	if path == "" {
		return nil
	}
	return execWithRetry(d,
		`UPDATE sessions SET transcript_path = ? WHERE id = ? AND transcript_path IS NULL`,
		path, sessionID,
	)
}

// CountEventsByType returns the count of events per type for a given session.
func CountEventsByType(db *sql.DB, sessionID string) (map[string]int, error) {
	const q = `SELECT type, COUNT(*) FROM events WHERE session_id=? GROUP BY type`
	rows, err := db.Query(q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("CountEventsByType: query: %w", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var typ string
		var count int
		if err := rows.Scan(&typ, &count); err != nil {
			return nil, fmt.Errorf("CountEventsByType: scan: %w", err)
		}
		result[typ] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("CountEventsByType: rows: %w", err)
	}
	return result, nil
}

// InsertArtifact inserts a new row into artifacts.
func InsertArtifact(db *sql.DB, a ArtifactRow) error {
	const q = `INSERT INTO artifacts (id, session_id, event_id, kind, path, content_hash, size_bytes, mime, metadata_json)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := db.Exec(q,
		a.ID, a.SessionID, a.EventID, a.Kind, a.Path,
		a.ContentHash, a.SizeBytes, a.Mime, a.MetadataJSON,
	)
	if err != nil {
		return fmt.Errorf("InsertArtifact: %w", err)
	}
	return nil
}

// UpdateArtifactStats sets size_bytes and content_hash on an existing artifact row.
func UpdateArtifactStats(db *sql.DB, artifactID string, sizeBytes int64, contentHash string) error {
	_, err := db.Exec(
		`UPDATE artifacts SET size_bytes=?, content_hash=? WHERE id=?`,
		sizeBytes, contentHash, artifactID,
	)
	if err != nil {
		return fmt.Errorf("UpdateArtifactStats: %w", err)
	}
	return nil
}

// ListArtifacts returns all artifact rows for a given session.
func ListArtifacts(db *sql.DB, sessionID string) ([]ArtifactRow, error) {
	const q = `SELECT id, session_id, event_id, kind, path, content_hash, size_bytes, mime, metadata_json
	FROM artifacts WHERE session_id=?`
	rows, err := db.Query(q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("ListArtifacts: query: %w", err)
	}
	defer rows.Close()

	var result []ArtifactRow
	for rows.Next() {
		var a ArtifactRow
		if err := rows.Scan(
			&a.ID, &a.SessionID, &a.EventID, &a.Kind, &a.Path,
			&a.ContentHash, &a.SizeBytes, &a.Mime, &a.MetadataJSON,
		); err != nil {
			return nil, fmt.Errorf("ListArtifacts: scan: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ListArtifacts: rows: %w", err)
	}
	return result, nil
}

// InsertSessionSummary inserts a new row into session_summary with the given status.
func InsertSessionSummary(db *sql.DB, sessionID, status string) error {
	_, err := db.Exec(
		`INSERT INTO session_summary (session_id, status) VALUES (?, ?)`,
		sessionID, status,
	)
	if err != nil {
		return fmt.Errorf("InsertSessionSummary: %w", err)
	}
	return nil
}

// UpdateSessionSummaryStatus updates the status column in session_summary.
func UpdateSessionSummaryStatus(db *sql.DB, sessionID, status string) error {
	_, err := db.Exec(
		`UPDATE session_summary SET status=? WHERE session_id=?`,
		status, sessionID,
	)
	if err != nil {
		return fmt.Errorf("UpdateSessionSummaryStatus: %w", err)
	}
	return nil
}

// errContains is a helper used in tests; exported here for internal reuse.
// It is intentionally unexported (lower-case) — tests in the same package can use it.
func errContains(err error, sub string) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(sub))
}
