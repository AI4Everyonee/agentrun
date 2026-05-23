package recorder

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
)

// newTestRecorder opens a fresh DB in a temp dir and starts a Recorder.
// The DB is closed at test cleanup. The caller is responsible for calling rec.Close.
func newTestRecorder(t *testing.T) (*Recorder, *sql.DB, string) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "agentrun.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	artDir := filepath.Join(tmp, "artifacts")
	rec, err := Start(d, StartOpts{
		Agent:        "claude",
		Cwd:          tmp,
		ArtifactsDir: artDir,
		Redactor:     NoopRedactor{},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return rec, d, tmp
}

// Test 1: Start creates session + summary + 2 artifact rows + session.started event.
func TestRecorder_StartCreatesExpectedRows(t *testing.T) {
	rec, d, tmp := newTestRecorder(t)
	defer rec.Close("", 0) //nolint:errcheck

	sessionID := rec.SessionID()

	// Session row.
	sess, err := db.GetSession(d, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Agent != "claude" {
		t.Errorf("Agent=%q, want %q", sess.Agent, "claude")
	}
	if sess.Cwd != tmp {
		t.Errorf("Cwd=%q, want %q", sess.Cwd, tmp)
	}

	// session.started event must be present synchronously.
	counts, err := db.CountEventsByType(d, sessionID)
	if err != nil {
		t.Fatalf("CountEventsByType: %v", err)
	}
	if counts["session.started"] != 1 {
		t.Errorf("session.started count=%d, want 1", counts["session.started"])
	}

	// Two artifact rows.
	arts, err := db.ListArtifacts(d, sessionID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("artifact count=%d, want 2", len(arts))
	}
	for _, a := range arts {
		if a.Kind != "terminal_log" {
			t.Errorf("artifact Kind=%q, want %q", a.Kind, "terminal_log")
		}
		if !a.Path.Valid || a.Path.String == "" {
			t.Errorf("artifact has empty path")
		}
	}

	// Verify pty.raw and stdin.raw paths exist on disk.
	if _, err := os.Stat(rec.PtyArtifactPath()); err != nil {
		t.Errorf("pty.raw not found on disk: %v", err)
	}
	if _, err := os.Stat(rec.StdinArtifactPath()); err != nil {
		t.Errorf("stdin.raw not found on disk: %v", err)
	}
}

// Test 2: Emit + Close persists all events.
func TestRecorder_EmitAndClosePersiststEvents(t *testing.T) {
	rec, d, _ := newTestRecorder(t)

	const n = 100
	payload := []byte(`{"bytes_b64":"aGVsbG8="}`)
	for i := 0; i < n; i++ {
		rec.Emit("pty", "terminal.output", payload)
	}

	if err := rec.Close("", 0); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Allow up to 2s for writer to fully drain (Close already waits but the
	// session.started was emitted synchronously, so we have n+1+1 events total).
	time.Sleep(500 * time.Millisecond)

	counts, err := db.CountEventsByType(d, rec.SessionID())
	if err != nil {
		t.Fatalf("CountEventsByType: %v", err)
	}
	if counts["terminal.output"] < n {
		t.Errorf("terminal.output count=%d, want >= %d", counts["terminal.output"], n)
	}
	if counts["session.started"] < 1 {
		t.Errorf("session.started count=%d, want >= 1", counts["session.started"])
	}
	if counts["session.ended"] < 1 {
		t.Errorf("session.ended count=%d, want >= 1", counts["session.ended"])
	}
}

// Test 3: EmitSync writes immediately — present before Close.
func TestRecorder_EmitSyncWritesImmediately(t *testing.T) {
	rec, d, _ := newTestRecorder(t)
	defer rec.Close("", 0) //nolint:errcheck

	if err := rec.EmitSync("system", "terminal.output", []byte(`{"test":true}`)); err != nil {
		t.Fatalf("EmitSync: %v", err)
	}

	counts, err := db.CountEventsByType(d, rec.SessionID())
	if err != nil {
		t.Fatalf("CountEventsByType: %v", err)
	}
	// terminal.output should already be in the DB (synchronous insert).
	if counts["terminal.output"] < 1 {
		t.Errorf("terminal.output count=%d, want >= 1 immediately after EmitSync", counts["terminal.output"])
	}
}

// Test 4: Close updates session row correctly (exitCode, ended_at, end_commit_sha, status).
func TestRecorder_CloseUpdatesSessionRow(t *testing.T) {
	rec, d, _ := newTestRecorder(t)

	if err := rec.Close("abc123", 2); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sess, err := db.GetSession(d, rec.SessionID())
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if !sess.ExitCode.Valid || sess.ExitCode.Int64 != 2 {
		t.Errorf("ExitCode=%v (valid=%v), want 2", sess.ExitCode.Int64, sess.ExitCode.Valid)
	}
	if !sess.EndedAt.Valid {
		t.Error("EndedAt is not valid, want a time")
	}
	if !sess.EndCommitSHA.Valid || sess.EndCommitSHA.String != "abc123" {
		t.Errorf("EndCommitSHA=%q (valid=%v), want %q", sess.EndCommitSHA.String, sess.EndCommitSHA.Valid, "abc123")
	}

	// session_summary.status should be "failed" for exitCode != 0.
	// We verify via CountEventsByType and GetSession; the FinalizeSession function
	// updates session_summary.status internally.
	// Query session_summary directly.
	var status string
	err = d.QueryRow(`SELECT status FROM session_summary WHERE session_id=?`, rec.SessionID()).Scan(&status)
	if err != nil {
		t.Fatalf("query session_summary: %v", err)
	}
	if status != "failed" {
		t.Errorf("session_summary.status=%q, want %q", status, "failed")
	}
}

// Test 5: Close is idempotent — second call returns same (nil) error, no panic.
func TestRecorder_CloseIdempotent(t *testing.T) {
	rec, _, _ := newTestRecorder(t)

	if err := rec.Close("", 0); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rec.Close("", 0); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// Test 6: UpdatePID round-trips correctly.
func TestRecorder_UpdatePID(t *testing.T) {
	rec, d, _ := newTestRecorder(t)
	defer rec.Close("", 0) //nolint:errcheck

	if err := rec.UpdatePID(12345); err != nil {
		t.Fatalf("UpdatePID: %v", err)
	}

	sess, err := db.GetSession(d, rec.SessionID())
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !sess.PID.Valid || sess.PID.Int64 != 12345 {
		t.Errorf("PID=%v (valid=%v), want 12345", sess.PID.Int64, sess.PID.Valid)
	}
}

// Test 7: Artifact stats are updated on Close after writing bytes to the pty path.
func TestRecorder_ArtifactStatsUpdatedOnClose(t *testing.T) {
	rec, d, _ := newTestRecorder(t)

	// Write some data to the pty artifact file before Close.
	content := []byte("some test output bytes for sha256 hashing")
	if err := os.WriteFile(rec.PtyArtifactPath(), content, 0o644); err != nil {
		t.Fatalf("WriteFile pty path: %v", err)
	}

	if err := rec.Close("", 0); err != nil {
		t.Fatalf("Close: %v", err)
	}

	arts, err := db.ListArtifacts(d, rec.SessionID())
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}

	var ptyArt *db.ArtifactRow
	for i := range arts {
		if arts[i].Path.Valid && filepath.Base(arts[i].Path.String) == "pty.raw" {
			ptyArt = &arts[i]
			break
		}
	}
	if ptyArt == nil {
		t.Fatal("pty.raw artifact not found")
	}

	if !ptyArt.SizeBytes.Valid || ptyArt.SizeBytes.Int64 <= 0 {
		t.Errorf("pty artifact SizeBytes=%v (valid=%v), want > 0", ptyArt.SizeBytes.Int64, ptyArt.SizeBytes.Valid)
	}
	if !ptyArt.ContentHash.Valid || len(ptyArt.ContentHash.String) != 64 {
		t.Errorf("pty artifact ContentHash=%q (len=%d), want 64-char hex", ptyArt.ContentHash.String, len(ptyArt.ContentHash.String))
	}
}
