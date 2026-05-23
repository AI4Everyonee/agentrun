package recorder

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jeevan/agentrun/internal/db"
	"github.com/jeevan/agentrun/internal/gitmeta"
	"github.com/jeevan/agentrun/internal/ids"
)

// Event is the in-memory representation of an event before insertion.
type Event struct {
	ID       string
	Sequence int64
	Ts       time.Time
	Source   string // "pty" | "system"
	Type     string // "terminal.output" | "terminal.stdin" | "session.started" | "session.ended"
	Payload  []byte // valid JSON
}

// StartOpts groups the inputs for Start.
type StartOpts struct {
	Agent          string
	AgentVersion   string
	Cwd            string
	RepoRoot       string
	Branch         string
	StartCommitSHA string
	ArtifactsDir   string // <db_dir>/artifacts
	Redactor       Redactor
}

// Recorder owns the event channel, writer goroutine, and session lifecycle.
type Recorder struct {
	db        *sql.DB
	sessionID string
	redactor  Redactor

	ch   chan Event
	done chan struct{} // closed when writer goroutine exits

	ptyArtifactID   string
	stdinArtifactID string
	ptyPath         string
	stdinPath       string

	// Captured at Start so Close can run git diff between start and end SHAs.
	cwd            string
	startCommitSHA string
	artDir         string

	closeOnce sync.Once
	closeErr  error
}

// Start creates the session, inserts the artifact rows, spawns the writer goroutine,
// and emits the session.started event synchronously. Returns a ready-to-use Recorder.
func Start(d *sql.DB, opts StartOpts) (*Recorder, error) {
	// Step 1: generate session ID.
	sessionID := ids.Session()

	// Step 2: build SessionRow.
	nullStr := func(s string) sql.NullString {
		return sql.NullString{String: s, Valid: s != ""}
	}
	sess := db.SessionRow{
		ID:             sessionID,
		Agent:          opts.Agent,
		AgentVersion:   nullStr(opts.AgentVersion),
		Cwd:            opts.Cwd,
		RepoRoot:       nullStr(opts.RepoRoot),
		Branch:         nullStr(opts.Branch),
		StartCommitSHA: nullStr(opts.StartCommitSHA),
		StartedAt:      time.Now().UTC(),
		MetadataJSON:   "{}",
	}

	// Step 3: insert session row.
	if err := db.InsertSession(d, sess); err != nil {
		return nil, fmt.Errorf("recorder.Start: insert session: %w", err)
	}

	// Step 4: insert session_summary row.
	if err := db.InsertSessionSummary(d, sessionID, "running"); err != nil {
		return nil, fmt.Errorf("recorder.Start: insert session summary: %w", err)
	}

	// Step 5: create artifacts directory.
	artDir := filepath.Join(opts.ArtifactsDir, sessionID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return nil, fmt.Errorf("recorder.Start: create artifacts dir: %w", err)
	}

	// Step 6: compute artifact paths.
	ptyPath := filepath.Join(artDir, "pty.raw")
	stdinPath := filepath.Join(artDir, "stdin.raw")

	// Step 7: touch both files so they exist even if no data flows.
	for _, p := range []string{ptyPath, stdinPath} {
		f, err := os.Create(p)
		if err != nil {
			return nil, fmt.Errorf("recorder.Start: create artifact file %q: %w", p, err)
		}
		f.Close()
	}

	// Step 8: insert artifact rows.
	ptyArtID := ids.Artifact()
	stdinArtID := ids.Artifact()

	for _, art := range []struct {
		id, path string
	}{
		{ptyArtID, ptyPath},
		{stdinArtID, stdinPath},
	} {
		row := db.ArtifactRow{
			ID:           art.id,
			SessionID:    sessionID,
			EventID:      sql.NullString{},
			Kind:         "terminal_log",
			Path:         sql.NullString{String: art.path, Valid: true},
			ContentHash:  sql.NullString{},
			SizeBytes:    sql.NullInt64{},
			Mime:         sql.NullString{String: "application/octet-stream", Valid: true},
			MetadataJSON: "{}",
		}
		if err := db.InsertArtifact(d, row); err != nil {
			return nil, fmt.Errorf("recorder.Start: insert artifact: %w", err)
		}
	}

	// Steps 9–10: create channel and spawn writer goroutine.
	r := &Recorder{
		db:              d,
		sessionID:       sessionID,
		redactor:        opts.Redactor,
		ch:              make(chan Event, 1024),
		done:            make(chan struct{}),
		ptyArtifactID:   ptyArtID,
		stdinArtifactID: stdinArtID,
		ptyPath:         ptyPath,
		stdinPath:       stdinPath,
		cwd:             opts.Cwd,
		startCommitSHA:  opts.StartCommitSHA,
		artDir:          artDir,
	}
	go r.run()

	// Step 11: synchronously emit session.started.
	payload := []byte(fmt.Sprintf(`{"agent":%q,"cwd":%q}`, opts.Agent, opts.Cwd))
	if err := r.EmitSync("system", "session.started", payload); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: failed to emit session.started: %v\n", err)
	}

	// Step 12: return ready recorder.
	return r, nil
}

// SessionID returns the session's ULID-prefixed ID.
func (r *Recorder) SessionID() string { return r.sessionID }

// PtyArtifactPath returns the absolute path the PTY copy goroutine should tee into.
func (r *Recorder) PtyArtifactPath() string { return r.ptyPath }

// StdinArtifactPath returns the absolute path the stdin copy goroutine should tee into.
func (r *Recorder) StdinArtifactPath() string { return r.stdinPath }

// Emit pushes an event onto the channel (non-blocking; drops with a one-line stderr
// warning if full — the on-disk artifact is the canonical fallback per plan R2).
// Event.Sequence is left zero here; the writer goroutine assigns sequences from
// MAX(sequence)+1 inside the batch transaction so PTY events coordinate
// correctly with hook subprocesses writing to the same DB.
func (r *Recorder) Emit(source, eventType string, payload []byte) {
	e := Event{
		ID:      ids.Event(),
		Ts:      time.Now().UTC(),
		Source:  source,
		Type:    eventType,
		Payload: append([]byte(nil), payload...), // defensive copy
	}
	select {
	case r.ch <- e:
	default:
		fmt.Fprintf(os.Stderr, "agentrun: event channel full, dropping %s (artifact preserves bytes)\n", eventType)
	}
}

// EmitSync inserts an event directly, bypassing the channel. Used for session.started/ended.
// Uses db.InsertEventWithAutoSeq so the cross-process retry envelope handles
// any collision with concurrent hook subprocesses.
func (r *Recorder) EmitSync(source, eventType string, payload []byte) error {
	redacted, version := r.redactor.Redact(eventType, payload)
	_, err := db.InsertEventWithAutoSeq(
		r.db, r.sessionID, time.Now().UTC(), source, eventType, redacted, version,
	)
	return err
}

// UpdatePID updates sessions.pid after the child is forked.
func (r *Recorder) UpdatePID(pid int) error {
	return db.UpdateSessionPID(r.db, r.sessionID, pid)
}

// Close finalizes the session. Idempotent. endCommitSHA may be "" if not in a git repo.
// status is "completed" if exitCode == 0, else "failed".
func (r *Recorder) Close(endCommitSHA string, exitCode int) error {
	r.closeOnce.Do(func() {
		// 1. Emit session.ended synchronously so it lands even if channel is congested.
		payload := []byte(fmt.Sprintf(`{"exit_code":%d}`, exitCode))
		if err := r.EmitSync("system", "session.ended", payload); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun: failed to emit session.ended: %v\n", err)
		}

		// 2. Close channel and wait for writer drain, max 2s.
		close(r.ch)
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
			fmt.Fprintln(os.Stderr, "agentrun: recorder shutdown timed out, events may be lost")
		}

		// 3. Compute artifact sizes + hashes from disk; update rows.
		for _, art := range []struct{ id, path string }{
			{r.ptyArtifactID, r.ptyPath},
			{r.stdinArtifactID, r.stdinPath},
		} {
			size, hash, err := fileSizeAndHash(art.path)
			if err != nil {
				// file may not exist if nothing was written; that's fine
				continue
			}
			if err := db.UpdateArtifactStats(r.db, art.id, size, hash); err != nil {
				fmt.Fprintf(os.Stderr, "agentrun: failed to update artifact %s: %v\n", art.id, err)
			}
		}

		// 4. Capture git diff between start and end SHAs as an artifact (best-effort).
		if r.startCommitSHA != "" {
			r.captureGitDiff(endCommitSHA)
		}

		// 5. Finalize the session row.
		status := "completed"
		if exitCode != 0 {
			status = "failed"
		}
		if err := db.FinalizeSession(r.db, r.sessionID, endCommitSHA, status, exitCode, time.Now().UTC()); err != nil {
			r.closeErr = err
			return
		}
	})
	return r.closeErr
}

// captureGitDiff runs git diff between startCommitSHA and endCommitSHA, writes
// the patch to <artDir>/git.diff, and inserts an artifacts row. Best-effort:
// any error is logged and ignored; finalization continues regardless.
//
// endCommitSHA may be empty (no end SHA captured) — in that case diff vs.
// working tree is taken via gitmeta.Diff's startRef-only path.
func (r *Recorder) captureGitDiff(endCommitSHA string) {
	diff := gitmeta.Diff(r.cwd, r.startCommitSHA, endCommitSHA)
	if diff == "" {
		return
	}
	path := filepath.Join(r.artDir, "git.diff")
	if err := os.WriteFile(path, []byte(diff), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: write git diff: %v\n", err)
		return
	}
	size, hash, err := fileSizeAndHash(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: stat git diff: %v\n", err)
		return
	}
	stat := gitmeta.DiffStat(r.cwd, r.startCommitSHA, endCommitSHA)
	metaJSON := fmt.Sprintf(`{"stat":%q}`, stat)
	row := db.ArtifactRow{
		ID:           ids.Artifact(),
		SessionID:    r.sessionID,
		EventID:      sql.NullString{},
		Kind:         "git_diff",
		Path:         sql.NullString{String: path, Valid: true},
		ContentHash:  sql.NullString{String: hash, Valid: true},
		SizeBytes:    sql.NullInt64{Int64: size, Valid: true},
		Mime:         sql.NullString{String: "text/x-diff", Valid: true},
		MetadataJSON: metaJSON,
	}
	if err := db.InsertArtifact(r.db, row); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: insert git_diff artifact: %v\n", err)
	}
}

// batchSize is the max number of events to accumulate before flushing.
const batchSize = 64

// flushInterval is the max time between flushes when events are present.
const flushInterval = 250 * time.Millisecond

// run is the single writer goroutine that drains the event channel.
func (r *Recorder) run() {
	defer close(r.done)
	defer func() {
		if rec := recover(); rec != nil {
			fmt.Fprintf(os.Stderr, "agentrun: recorder writer panicked: %v\n", rec)
		}
	}()

	buf := make([]Event, 0, batchSize)
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()

	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := r.writeBatch(buf); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun: event flush failed: %v\n", err)
		}
		buf = buf[:0]
	}

	for {
		select {
		case e, ok := <-r.ch:
			if !ok {
				flush()
				return
			}
			buf = append(buf, e)
			if len(buf) >= batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// writeBatch persists a batch of events within a single transaction.
// Sequences are assigned as MAX(existing) + 1 + offset inside the transaction
// to coordinate with hook subprocesses writing to the same session. On a
// concurrency error (SQLITE_BUSY or UNIQUE-sequence collision from a peer
// writer that landed between our MAX read and our INSERT), retries the
// whole batch up to 3 times with backoff.
func (r *Recorder) writeBatch(batch []Event) error {
	backoffs := []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond}
	var lastErr error
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		err := r.tryWriteBatch(batch)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableWriteErr(err) {
			return err
		}
		if attempt == len(backoffs) {
			break
		}
		time.Sleep(backoffs[attempt])
	}
	return fmt.Errorf("writeBatch: exhausted retries: %w", lastErr)
}

func (r *Recorder) tryWriteBatch(batch []Event) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("writeBatch: begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var nextSeq int64
	if err := tx.QueryRow(
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE session_id = ?`,
		r.sessionID,
	).Scan(&nextSeq); err != nil {
		return fmt.Errorf("writeBatch: scan max(seq): %w", err)
	}

	stmt, err := db.InsertEventStmt(tx)
	if err != nil {
		return fmt.Errorf("writeBatch: prepare stmt: %w", err)
	}

	fileModifications := 0
	for _, e := range batch {
		nextSeq++
		redacted, version := r.redactor.Redact(e.Type, e.Payload)
		_, err = stmt.Exec(
			e.ID, r.sessionID, nextSeq,
			e.Ts.UTC().Format(time.RFC3339Nano),
			e.Source, e.Type, redacted, version,
		)
		if err != nil {
			stmt.Close()
			return fmt.Errorf("writeBatch: exec: %w", err)
		}
		if e.Type == "file.modified" {
			fileModifications++
		}
	}

	stmt.Close()

	// Roll up filesystem changes into the session_summary counter in the same
	// transaction so a partial commit can't leave the counter desynced.
	if fileModifications > 0 {
		if _, err := tx.Exec(
			`UPDATE session_summary SET files_changed = files_changed + ? WHERE session_id = ?`,
			fileModifications, r.sessionID,
		); err != nil {
			return fmt.Errorf("writeBatch: bump files_changed: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("writeBatch: commit: %w", err)
	}
	committed = true
	return nil
}

// isRetryableWriteErr reports whether err is one of the recoverable
// concurrency errors we expect when peer hook processes write events
// to the same session. Mirrors db.isRetryableInsertErr but duplicated
// here to avoid exporting from the db package.
func isRetryableWriteErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "unique constraint failed") ||
		strings.Contains(msg, "constraint failed: events.session_id, events.sequence")
}

// fileSizeAndHash opens path, streams it through SHA-256, and returns
// the total byte count and lowercase hex-encoded digest.
func fileSizeAndHash(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}
