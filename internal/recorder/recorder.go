package recorder

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jeevan/agentrun/internal/db"
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
	seq  int64        // accessed via atomic.AddInt64

	ptyArtifactID   string
	stdinArtifactID string
	ptyPath         string
	stdinPath       string

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
func (r *Recorder) Emit(source, eventType string, payload []byte) {
	e := Event{
		ID:       ids.Event(),
		Sequence: atomic.AddInt64(&r.seq, 1),
		Ts:       time.Now().UTC(),
		Source:   source,
		Type:     eventType,
		Payload:  append([]byte(nil), payload...), // defensive copy
	}
	select {
	case r.ch <- e:
	default:
		fmt.Fprintf(os.Stderr, "agentrun: event channel full, dropping %s (artifact preserves bytes)\n", eventType)
	}
}

// EmitSync inserts an event directly, bypassing the channel. Used for session.started/ended.
func (r *Recorder) EmitSync(source, eventType string, payload []byte) error {
	seq := atomic.AddInt64(&r.seq, 1)
	redacted, version := r.redactor.Redact(eventType, payload)
	row := db.EventRow{
		ID:               ids.Event(),
		SessionID:        r.sessionID,
		Sequence:         seq,
		Ts:               time.Now().UTC(),
		Source:           source,
		Type:             eventType,
		PayloadJSON:      redacted,
		RedactionVersion: sql.NullString{String: version, Valid: true},
	}
	return db.InsertEventOne(r.db, row)
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

		// 4. Finalize the session row.
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
func (r *Recorder) writeBatch(batch []Event) error {
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("writeBatch: begin tx: %w", err)
	}

	stmt, err := db.InsertEventStmt(tx)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writeBatch: prepare stmt: %w", err)
	}

	for _, e := range batch {
		redacted, version := r.redactor.Redact(e.Type, e.Payload)
		_, err = stmt.Exec(
			e.ID, r.sessionID, e.Sequence,
			e.Ts.UTC().Format(time.RFC3339Nano),
			e.Source, e.Type, redacted, version,
		)
		if err != nil {
			stmt.Close()
			_ = tx.Rollback()
			return fmt.Errorf("writeBatch: exec: %w", err)
		}
	}

	stmt.Close()
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("writeBatch: commit: %w", err)
	}
	return nil
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
