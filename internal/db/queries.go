package db

import (
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/ids"
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

	// Added by migrations (see internal/db/migrations.go):
	//   v1: user_name           — capture identity for multi-user cloud syncs
	//   v2: tokens_used,        — token + cost rollup (parsed from PTY where possible)
	//       cost_usd_cents
	//   v5: summary, summary_model, summary_tokens — OpenAI session summary
	UserName      sql.NullString
	TokensUsed    sql.NullInt64
	CostUSDCents  sql.NullInt64
	Summary       sql.NullString
	SummaryModel  sql.NullString
	SummaryTokens sql.NullInt64
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
	UserName  sql.NullString
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
		started_at, ended_at, exit_code, transcript_path, pid, metadata_json,
		user_name, tokens_used, cost_usd_cents,
		summary, summary_model, summary_tokens
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	var endedAt interface{}
	if s.EndedAt.Valid {
		endedAt = fmtTime(s.EndedAt.Time)
	}

	_, err := db.Exec(q,
		s.ID, s.Agent, s.AgentVersion, s.Model, s.PermissionMode,
		s.Cwd, s.RepoRoot, s.Branch, s.StartCommitSHA, s.EndCommitSHA,
		fmtTime(s.StartedAt), endedAt, s.ExitCode, s.TranscriptPath, s.PID,
		s.MetadataJSON,
		s.UserName, s.TokensUsed, s.CostUSDCents,
		s.Summary, s.SummaryModel, s.SummaryTokens,
	)
	if err != nil {
		return fmt.Errorf("InsertSession: %w", err)
	}
	return nil
}

// UpdateSessionSummary writes the OpenAI-generated summary + metadata. Empty
// summary skips. Same retry envelope as other Update helpers.
func UpdateSessionSummary(d *sql.DB, sessionID, summary, model string, tokens int64) error {
	if summary == "" {
		return nil
	}
	return execWithRetry(d,
		`UPDATE sessions
		    SET summary = ?, summary_model = ?, summary_tokens = ?
		  WHERE id = ?`,
		summary, model, tokens, sessionID,
	)
}

// UpdateSessionUserName sets sessions.user_name. Idempotent (overwrites).
// No-op on empty value. Useful when the wrapper doesn't know the user at
// row-creation time but a later hook payload reveals it.
func UpdateSessionUserName(d *sql.DB, sessionID, userName string) error {
	if userName == "" {
		return nil
	}
	return execWithRetry(d, `UPDATE sessions SET user_name = ? WHERE id = ?`, userName, sessionID)
}

// UpdateSessionTokensAndCost overwrites the rollup columns. Either value may
// be -1 to leave the existing value untouched.
func UpdateSessionTokensAndCost(d *sql.DB, sessionID string, tokens, costCents int64) error {
	switch {
	case tokens < 0 && costCents < 0:
		return nil
	case tokens >= 0 && costCents >= 0:
		return execWithRetry(d,
			`UPDATE sessions SET tokens_used = ?, cost_usd_cents = ? WHERE id = ?`,
			tokens, costCents, sessionID,
		)
	case tokens >= 0:
		return execWithRetry(d,
			`UPDATE sessions SET tokens_used = ? WHERE id = ?`,
			tokens, sessionID,
		)
	default:
		return execWithRetry(d,
			`UPDATE sessions SET cost_usd_cents = ? WHERE id = ?`,
			costCents, sessionID,
		)
	}
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
		SELECT s.id, s.agent, s.repo_root, s.cwd, s.started_at, ss.status, s.user_name
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
		if err := rows.Scan(&r.ID, &r.Agent, &r.RepoRoot, &r.Cwd, &startedAt, &r.Status, &r.UserName); err != nil {
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
		started_at, ended_at, exit_code, transcript_path, pid, metadata_json,
		user_name, tokens_used, cost_usd_cents,
		summary, summary_model, summary_tokens
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
		&s.UserName, &s.TokensUsed, &s.CostUSDCents,
		&s.Summary, &s.SummaryModel, &s.SummaryTokens,
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

// FetchSessionEvents returns all events for the session, sorted by sequence ascending.
// Used for export and turn-grouping in show.
func FetchSessionEvents(d *sql.DB, sessionID string) ([]EventRow, error) {
	const q = `SELECT id, session_id, sequence, ts, source, type, payload_json, redaction_version
	FROM events WHERE session_id=? ORDER BY sequence ASC`

	rows, err := d.Query(q, sessionID)
	if err != nil {
		return nil, fmt.Errorf("FetchSessionEvents: query: %w", err)
	}
	defer rows.Close()

	var result []EventRow
	for rows.Next() {
		var e EventRow
		var tsStr string
		if err := rows.Scan(
			&e.ID, &e.SessionID, &e.Sequence, &tsStr, &e.Source, &e.Type,
			&e.PayloadJSON, &e.RedactionVersion,
		); err != nil {
			return nil, fmt.Errorf("FetchSessionEvents: scan: %w", err)
		}
		e.Ts, err = parseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("FetchSessionEvents: parse ts: %w", err)
		}
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FetchSessionEvents: rows: %w", err)
	}
	return result, nil
}

// SearchHit is one result row from SearchEvents.
type SearchHit struct {
	EventID   string
	SessionID string
	Agent     string
	Type      string
	Ts        time.Time
	Snippet   string
}

// SearchEvents runs an FTS5 query against events_fts.
// sessionFilter and typeFilter may be empty strings to skip those constraints.
// limit must be > 0.
//
// Note: the FTS5 content table maps the FTS 'payload' column to events.payload_json.
// Because the column names differ, snippet() cannot be used (it would look for a
// column named 'payload' in the base table). Instead we return events.payload_json
// as the Snippet field and let the caller trim/highlight it.
func SearchEvents(d *sql.DB, query, sessionFilter, typeFilter string, limit int) ([]SearchHit, error) {
	base := `
SELECT
    e.id,
    e.session_id,
    s.agent,
    e.type,
    e.ts,
    CAST(e.payload_json AS TEXT) AS snip
FROM events_fts
JOIN events e ON e.rowid = events_fts.rowid
JOIN sessions s ON s.id = e.session_id
WHERE events_fts MATCH ?`

	args := []interface{}{query}

	if sessionFilter != "" {
		base += ` AND e.session_id = ?`
		args = append(args, sessionFilter)
	}
	if typeFilter != "" {
		base += ` AND e.type LIKE ?`
		args = append(args, "%"+typeFilter+"%")
	}

	base += ` ORDER BY e.ts DESC LIMIT ?`
	args = append(args, limit)

	rows, err := d.Query(base, args...)
	if err != nil {
		return nil, fmt.Errorf("SearchEvents: query: %w", err)
	}
	defer rows.Close()

	var result []SearchHit
	for rows.Next() {
		var h SearchHit
		var tsStr string
		if err := rows.Scan(&h.EventID, &h.SessionID, &h.Agent, &h.Type, &tsStr, &h.Snippet); err != nil {
			return nil, fmt.Errorf("SearchEvents: scan: %w", err)
		}
		h.Ts, err = parseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("SearchEvents: parse ts: %w", err)
		}
		result = append(result, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("SearchEvents: rows: %w", err)
	}
	return result, nil
}

// FinalizeIdleSessions marks running sessions as completed if their last event
// is older than olderThan ago. Returns the IDs of finalized sessions so
// callers (e.g. `agentrun finalize-idle`) can schedule downstream work like
// summarization. Both UPDATEs happen in one transaction.
func FinalizeIdleSessions(d *sql.DB, olderThan time.Duration) ([]string, error) {
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339Nano)

	// Collect the IDs that qualify BEFORE running the UPDATEs so we can
	// return them. The same predicate is then reused in the UPDATE statements.
	const selectIDs = `
WITH last_event AS (
    SELECT session_id, MAX(ts) AS last_ts
      FROM events
     GROUP BY session_id
)
SELECT s.id FROM sessions s
  JOIN session_summary ss ON ss.session_id = s.id
  LEFT JOIN last_event le ON le.session_id = s.id
 WHERE ss.status = 'running'
   AND (le.last_ts IS NULL OR le.last_ts < ?)`

	rows, err := d.Query(selectIDs, cutoff)
	if err != nil {
		return nil, fmt.Errorf("FinalizeIdleSessions: select ids: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("FinalizeIdleSessions: scan id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("FinalizeIdleSessions: rows: %w", err)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	tx, err := d.Begin()
	if err != nil {
		return nil, fmt.Errorf("FinalizeIdleSessions: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	const updateSessions = `
WITH last_event AS (
    SELECT session_id, MAX(ts) AS last_ts
      FROM events
     GROUP BY session_id
)
UPDATE sessions
   SET ended_at = COALESCE(ended_at, (SELECT last_ts FROM last_event WHERE session_id = sessions.id))
 WHERE id IN (
     SELECT s.id FROM sessions s
       JOIN session_summary ss ON ss.session_id = s.id
       LEFT JOIN last_event le ON le.session_id = s.id
      WHERE ss.status = 'running'
        AND (le.last_ts IS NULL OR le.last_ts < ?)
 )`
	if _, err = tx.Exec(updateSessions, cutoff); err != nil {
		return nil, fmt.Errorf("FinalizeIdleSessions: update sessions: %w", err)
	}

	const updateSummary = `
WITH last_event AS (
    SELECT session_id, MAX(ts) AS last_ts
      FROM events
     GROUP BY session_id
)
UPDATE session_summary SET status = 'completed'
 WHERE session_id IN (
     SELECT s.id FROM sessions s
       JOIN session_summary ss ON ss.session_id = s.id
       LEFT JOIN last_event le ON le.session_id = s.id
      WHERE ss.status = 'running'
        AND (le.last_ts IS NULL OR le.last_ts < ?)
 )`
	if _, err = tx.Exec(updateSummary, cutoff); err != nil {
		return nil, fmt.Errorf("FinalizeIdleSessions: update session_summary: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("FinalizeIdleSessions: commit: %w", err)
	}
	committed = true
	return ids, nil
}

// ─── stats helpers ────────────────────────────────────────────────────────────

// AgentStat holds per-agent usage numbers for `agentrun stats`.
type AgentStat struct {
	Agent           string
	SessionCount    int
	ToolCalls       int
	ValidationsFail int
	BytesEstimate   int64 // rough — sum of artifact size_bytes
}

// StatsByAgent returns per-agent rollups. All parameters are optional filters:
//   - repo: filter to a single repo_root (empty = all)
//   - user: filter to a single user_name (empty = all)
//   - since: zero value = all time; non-zero = only sessions started after this time
func StatsByAgent(d *sql.DB, repo, user string, since time.Time) ([]AgentStat, error) {
	args := []interface{}{}
	where := "1=1"
	if repo != "" {
		where += " AND s.repo_root = ?"
		args = append(args, repo)
	}
	if user != "" {
		where += " AND s.user_name = ?"
		args = append(args, user)
	}
	if !since.IsZero() {
		where += " AND s.started_at >= ?"
		args = append(args, since.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}

	q := fmt.Sprintf(`
SELECT s.agent,
       COUNT(DISTINCT s.id)                  AS session_count,
       COALESCE(SUM(ss.tool_calls), 0)       AS tool_calls,
       COALESCE(SUM(ss.validations_fail), 0) AS validations_fail,
       COALESCE((
           SELECT SUM(a.size_bytes)
           FROM artifacts a
           JOIN sessions s2 ON a.session_id = s2.id
           WHERE s2.agent = s.agent
           AND %s
       ), 0)                                 AS bytes_estimate
FROM sessions s
LEFT JOIN session_summary ss ON ss.session_id = s.id
WHERE %s
GROUP BY s.agent
ORDER BY session_count DESC`, where, where)

	// We use the same where/args twice — duplicate them.
	doubledArgs := append(append([]interface{}{}, args...), args...)

	rows, err := d.Query(q, doubledArgs...)
	if err != nil {
		return nil, fmt.Errorf("StatsByAgent: query: %w", err)
	}
	defer rows.Close()

	var result []AgentStat
	for rows.Next() {
		var a AgentStat
		if err := rows.Scan(&a.Agent, &a.SessionCount, &a.ToolCalls, &a.ValidationsFail, &a.BytesEstimate); err != nil {
			return nil, fmt.Errorf("StatsByAgent: scan: %w", err)
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

// RepoStat holds per-repo usage numbers for `agentrun stats`.
type RepoStat struct {
	RepoRoot     string
	SessionCount int
	ToolCalls    int
}

// StatsByRepo returns per-repo rollups, ordered by session count descending.
func StatsByRepo(d *sql.DB, agent, user string, since time.Time, limit int) ([]RepoStat, error) {
	args := []interface{}{}
	where := "1=1"
	if agent != "" {
		where += " AND s.agent = ?"
		args = append(args, agent)
	}
	if user != "" {
		where += " AND s.user_name = ?"
		args = append(args, user)
	}
	if !since.IsZero() {
		where += " AND s.started_at >= ?"
		args = append(args, since.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	args = append(args, limit)

	q := fmt.Sprintf(`
SELECT COALESCE(s.repo_root, '(no repo)')  AS repo_root,
       COUNT(DISTINCT s.id)                AS session_count,
       COALESCE(SUM(ss.tool_calls), 0)     AS tool_calls
FROM sessions s
LEFT JOIN session_summary ss ON ss.session_id = s.id
WHERE %s
GROUP BY COALESCE(s.repo_root, '(no repo)')
ORDER BY session_count DESC
LIMIT ?`, where)

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("StatsByRepo: query: %w", err)
	}
	defer rows.Close()

	var result []RepoStat
	for rows.Next() {
		var r RepoStat
		if err := rows.Scan(&r.RepoRoot, &r.SessionCount, &r.ToolCalls); err != nil {
			return nil, fmt.Errorf("StatsByRepo: scan: %w", err)
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// UserStat holds per-user session counts for `agentrun stats`.
type UserStat struct {
	UserName     string
	SessionCount int
}

// StatsByUser returns per-user rollups, ordered by session count descending.
func StatsByUser(d *sql.DB, limit int) ([]UserStat, error) {
	const q = `
SELECT COALESCE(user_name, '(unknown)') AS user_name,
       COUNT(*)                          AS session_count
FROM sessions
GROUP BY COALESCE(user_name, '(unknown)')
ORDER BY session_count DESC
LIMIT ?`

	rows, err := d.Query(q, limit)
	if err != nil {
		return nil, fmt.Errorf("StatsByUser: query: %w", err)
	}
	defer rows.Close()

	var result []UserStat
	for rows.Next() {
		var u UserStat
		if err := rows.Scan(&u.UserName, &u.SessionCount); err != nil {
			return nil, fmt.Errorf("StatsByUser: scan: %w", err)
		}
		result = append(result, u)
	}
	return result, rows.Err()
}

// ValidationFailure describes a session where validations failed, used in stats.
type ValidationFailure struct {
	SessionID string
	Command   string
	FailedAt  time.Time
}

// RecentValidationFailures returns sessions with validations_fail > 0, limited
// to those started after since (zero = all time), ordered by most-recent first.
func RecentValidationFailures(d *sql.DB, since time.Time, limit int) ([]ValidationFailure, error) {
	args := []interface{}{}
	where := "ss.validations_fail > 0"
	if !since.IsZero() {
		where += " AND s.started_at >= ?"
		args = append(args, since.UTC().Format("2006-01-02T15:04:05.999999999Z"))
	}
	args = append(args, limit)

	q := fmt.Sprintf(`
SELECT s.id, COALESCE(s.cwd, ''), s.started_at
FROM sessions s
JOIN session_summary ss ON ss.session_id = s.id
WHERE %s
ORDER BY s.started_at DESC
LIMIT ?`, where)

	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("RecentValidationFailures: query: %w", err)
	}
	defer rows.Close()

	var result []ValidationFailure
	for rows.Next() {
		var vf ValidationFailure
		var tsStr string
		if err := rows.Scan(&vf.SessionID, &vf.Command, &tsStr); err != nil {
			return nil, fmt.Errorf("RecentValidationFailures: scan: %w", err)
		}
		vf.FailedAt, err = parseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("RecentValidationFailures: parse ts: %w", err)
		}
		result = append(result, vf)
	}
	return result, rows.Err()
}
