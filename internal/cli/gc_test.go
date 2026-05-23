package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// newGCTestEnv creates a temp directory acting as the agentrun DB dir,
// opens a DB in it, and sets AGENTRUN_DB_DIR so runGC picks it up.
// Returns the db handle and the artifacts dir path.
func newGCTestEnv(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AGENTRUN_DB_DIR", dir)

	dbPath := filepath.Join(dir, "agentrun.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	artifactsDir := filepath.Join(dir, "artifacts")
	return d, artifactsDir
}

// seedSession inserts a session + session_summary row with the given started_at and status.
func seedSession(t *testing.T, d *sql.DB, id string, startedAt time.Time, status string) {
	t.Helper()
	sess := db.SessionRow{
		ID:           id,
		Agent:        "claude",
		Cwd:          "/tmp/test",
		StartedAt:    startedAt,
		MetadataJSON: "{}",
	}
	if err := db.InsertSession(d, sess); err != nil {
		t.Fatalf("InsertSession %s: %v", id, err)
	}
	if err := db.InsertSessionSummary(d, id, status); err != nil {
		t.Fatalf("InsertSessionSummary %s: %v", id, err)
	}
}

// sessionExists reports whether a sessions row with the given id is present in d.
func sessionExists(t *testing.T, d *sql.DB, id string) bool {
	t.Helper()
	var count int
	if err := d.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id=?`, id).Scan(&count); err != nil {
		t.Fatalf("sessionExists query for %s: %v", id, err)
	}
	return count > 0
}

// captureStdout captures what f writes to os.Stdout and returns it as a string.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w

	done := make(chan string)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	f()
	w.Close()
	os.Stdout = old
	return <-done
}

// TestGC_DeletesOldCompletedSessions verifies that a session older than the
// cutoff is deleted and a newer session is left intact.
func TestGC_DeletesOldCompletedSessions(t *testing.T) {
	d, _ := newGCTestEnv(t)

	now := time.Now().UTC()
	old := now.Add(-60 * 24 * time.Hour) // 60 days ago
	recent := now.Add(-1 * 24 * time.Hour) // 1 day ago

	seedSession(t, d, "s_old", old, "completed")
	seedSession(t, d, "s_new", recent, "completed")

	out := captureStdout(t, func() {
		if err := runGC([]string{"--older-than=30d"}); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})

	if sessionExists(t, d, "s_old") {
		t.Error("expected old session to be deleted")
	}
	if !sessionExists(t, d, "s_new") {
		t.Error("expected new session to remain")
	}
	if !strings.Contains(out, "deleted 1 session") {
		t.Errorf("expected output to mention 'deleted 1 session', got: %q", out)
	}
}

// TestGC_DryRun verifies that --dry-run does not actually delete anything
// and reports the correct count in its output.
func TestGC_DryRun(t *testing.T) {
	d, _ := newGCTestEnv(t)

	now := time.Now().UTC()
	old := now.Add(-60 * 24 * time.Hour)
	recent := now.Add(-1 * 24 * time.Hour)

	seedSession(t, d, "s_old", old, "completed")
	seedSession(t, d, "s_new", recent, "completed")

	out := captureStdout(t, func() {
		if err := runGC([]string{"--older-than=30d", "--dry-run"}); err != nil {
			t.Fatalf("runGC --dry-run: %v", err)
		}
	})

	// Both sessions must still exist.
	if !sessionExists(t, d, "s_old") {
		t.Error("--dry-run should not delete old session")
	}
	if !sessionExists(t, d, "s_new") {
		t.Error("--dry-run should not delete new session")
	}
	if !strings.Contains(out, "would delete 1") {
		t.Errorf("expected 'would delete 1' in output, got: %q", out)
	}
}

// TestGC_SkipsRunningSessions verifies that sessions with status='running' are
// not deleted even when they are older than the cutoff.
func TestGC_SkipsRunningSessions(t *testing.T) {
	d, _ := newGCTestEnv(t)

	now := time.Now().UTC()
	old := now.Add(-60 * 24 * time.Hour)

	seedSession(t, d, "s_running_old", old, "running")

	out := captureStdout(t, func() {
		if err := runGC([]string{"--older-than=30d"}); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})

	if !sessionExists(t, d, "s_running_old") {
		t.Error("expected running session to be preserved, but it was deleted")
	}
	if !strings.Contains(out, "deleted 0") {
		t.Errorf("expected 'deleted 0' in output, got: %q", out)
	}
}

// TestGC_RemovesArtifactsOnDisk verifies that on-disk artifact directories are
// removed when their associated session is deleted.
func TestGC_RemovesArtifactsOnDisk(t *testing.T) {
	d, artifactsDir := newGCTestEnv(t)

	now := time.Now().UTC()
	old := now.Add(-60 * 24 * time.Hour)
	sid := "s_art_old"

	seedSession(t, d, sid, old, "completed")

	// Create a fake artifact directory and file.
	artDir := filepath.Join(artifactsDir, sid)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	artFile := filepath.Join(artDir, "foo.txt")
	if err := os.WriteFile(artFile, []byte("artifact content"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify the directory exists before gc.
	if _, err := os.Stat(artDir); err != nil {
		t.Fatalf("artDir should exist before gc: %v", err)
	}

	captureStdout(t, func() {
		if err := runGC([]string{"--older-than=30d"}); err != nil {
			t.Fatalf("runGC: %v", err)
		}
	})

	// The artifact directory should be gone.
	if _, err := os.Stat(artDir); !os.IsNotExist(err) {
		t.Errorf("artifact directory should have been removed, err=%v", err)
	}
}

// TestParseDurationDaysSuffix tests the custom duration parser.
func TestParseDurationDaysSuffix(t *testing.T) {
	cases := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1d", 24 * time.Hour, false},
		{"0d", 0, false},
		{"1d12h", 36 * time.Hour, false},
		{"1h", time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"90s", 90 * time.Second, false},
		{"invalid", 0, true},
		{"xd", 0, true},
		{"-1d", 0, true},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := parseDurationWithDays(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseDurationWithDays(%q) = %v, want error", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Errorf("parseDurationWithDays(%q) unexpected error: %v", tc.input, err)
				return
			}
			if got != tc.want {
				t.Errorf("parseDurationWithDays(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestGC_NoExtraArgs ensures extra positional args return ErrUsage.
func TestGC_NoExtraArgs(t *testing.T) {
	err := runGC([]string{"extra"})
	if err == nil {
		t.Fatal("expected ErrUsage for extra args, got nil")
	}
}

// TestGC_InvalidOlderThan verifies that an invalid --older-than returns an error.
func TestGC_InvalidOlderThan(t *testing.T) {
	_ = newGCTestEnv // set env

	// We still need the env set — but we just want to check argument parsing error.
	dir := t.TempDir()
	t.Setenv("AGENTRUN_DB_DIR", dir)
	// Pre-create DB so Open succeeds.
	dbPath := filepath.Join(dir, "agentrun.db")
	dd, _ := db.Open(dbPath)
	dd.Close()

	err := runGC([]string{"--older-than=notaduration"})
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
	if !strings.Contains(err.Error(), "invalid --older-than") {
		t.Errorf("error should mention 'invalid --older-than', got: %v", err)
	}
}

// Compile-time check: ensure the helper builds (no unused import etc.)
var _ = fmt.Sprintf
