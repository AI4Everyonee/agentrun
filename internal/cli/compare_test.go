package cli

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/ids"
)

// seedCompareSessions inserts two sessions each with 3 events, the last event
// of each session having a different type to create at least one differing row.
func seedCompareSessions(t *testing.T) (idA, idB, dbPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	dbPathLocal := dir + "/compare_test.db"

	d, err := db.Open(dbPathLocal)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	now := time.Now().UTC()

	makeSession := func(agent, repo string) string {
		sid := ids.Session()
		sess := db.SessionRow{
			ID:           sid,
			Agent:        agent,
			Cwd:          repo,
			RepoRoot:     sql.NullString{String: repo, Valid: true},
			StartedAt:    now,
			MetadataJSON: "{}",
		}
		if err := db.InsertSession(d, sess); err != nil {
			t.Fatalf("InsertSession: %v", err)
		}
		if err := db.InsertSessionSummary(d, sid, "completed"); err != nil {
			t.Fatalf("InsertSessionSummary: %v", err)
		}
		return sid
	}

	sidA := makeSession("claude", "/repo/x")
	sidB := makeSession("codex", "/repo/x")

	insertEvent := func(sid string, seq int64, evType string) {
		e := db.EventRow{
			ID:          ids.Event(),
			SessionID:   sid,
			Sequence:    seq,
			Ts:          now.Add(time.Duration(seq) * time.Second),
			Source:      "hook",
			Type:        evType,
			PayloadJSON: []byte(`{}`),
		}
		if err := db.InsertEventOne(d, e); err != nil {
			t.Fatalf("InsertEventOne(%s seq=%d): %v", sid, seq, err)
		}
	}

	// Session A: hook.session_start, tool.pre_use, response.stopped
	insertEvent(sidA, 1, "hook.session_start")
	insertEvent(sidA, 2, "tool.pre_use")
	insertEvent(sidA, 3, "response.stopped")

	// Session B: hook.session_start, tool.pre_use, hook.session_end (differs from A)
	insertEvent(sidB, 1, "hook.session_start")
	insertEvent(sidB, 2, "tool.pre_use")
	insertEvent(sidB, 3, "hook.session_end")

	return sidA, sidB, dbPathLocal, func() { d.Close() }
}

func TestRunCompare_SideBySide(t *testing.T) {
	idA, idB, dbPath, cleanup := seedCompareSessions(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	out := captureStdout(t, func() {
		if err := runCompare([]string{idA, idB}); err != nil {
			t.Fatalf("runCompare: %v", err)
		}
	})

	s := string(out)

	// Both session IDs should appear in the header.
	if !strings.Contains(s, idA[:10]) {
		t.Errorf("output missing session A ID prefix:\n%s", s)
	}
	if !strings.Contains(s, idB[:10]) {
		t.Errorf("output missing session B ID prefix:\n%s", s)
	}

	// Should contain event type labels.
	if !strings.Contains(s, "hook.session_start") {
		t.Errorf("output missing 'hook.session_start':\n%s", s)
	}
	if !strings.Contains(s, "tool.pre_use") {
		t.Errorf("output missing 'tool.pre_use':\n%s", s)
	}

	// Row 3 should show the differing events.
	if !strings.Contains(s, "response.stopped") {
		t.Errorf("output missing 'response.stopped':\n%s", s)
	}
	if !strings.Contains(s, "hook.session_end") {
		t.Errorf("output missing 'hook.session_end':\n%s", s)
	}
}

func TestRunCompare_UnknownSession(t *testing.T) {
	_, _, dbPath, cleanup := seedCompareSessions(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	err := runCompare([]string{"s_notexist_aaaa", "s_notexist_bbbb"})
	if err == nil {
		t.Fatal("expected error for non-existent sessions, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

func TestRunCompare_ErrUsage(t *testing.T) {
	err := runCompare([]string{"only_one_arg"})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage, got %v", err)
	}
}
