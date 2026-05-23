package cli

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jeevan/agentrun/internal/db"
	"github.com/jeevan/agentrun/internal/ids"
)

// seedStatsDB creates 4 sessions (2 claude in /a and /b, 2 codex in /a),
// adds session_summary rows, and inserts 5 tool.pre_use events total.
// Returns the db path and the opened *sql.DB.
func seedStatsDB(t *testing.T) (dbPath string, d *sql.DB, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	dbPathLocal := dir + "/stats_test.db"

	dLocal, err := db.Open(dbPathLocal)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	now := time.Now().UTC()

	type sessionSpec struct {
		id    string
		agent string
		repo  string
		user  string
	}

	specs := []sessionSpec{
		{ids.Session(), "claude", "/a", "alice"},
		{ids.Session(), "claude", "/b", "alice"},
		{ids.Session(), "codex", "/a", "bob"},
		{ids.Session(), "codex", "/a", "bob"},
	}

	for _, sp := range specs {
		sess := db.SessionRow{
			ID:           sp.id,
			Agent:        sp.agent,
			Cwd:          sp.repo,
			RepoRoot:     sql.NullString{String: sp.repo, Valid: true},
			UserName:     sql.NullString{String: sp.user, Valid: true},
			StartedAt:    now,
			MetadataJSON: "{}",
		}
		if err := db.InsertSession(dLocal, sess); err != nil {
			t.Fatalf("InsertSession(%s): %v", sp.id, err)
		}
		if err := db.InsertSessionSummary(dLocal, sp.id, "completed"); err != nil {
			t.Fatalf("InsertSessionSummary(%s): %v", sp.id, err)
		}
	}

	// Insert 5 tool.pre_use events: 3 for session 0, 2 for session 2.
	eventCounts := []struct {
		sessIdx int
		n       int
	}{
		{0, 3},
		{2, 2},
	}
	evtSeq := 0
	for _, ec := range eventCounts {
		sid := specs[ec.sessIdx].id
		for j := 0; j < ec.n; j++ {
			evtSeq++
			e := db.EventRow{
				ID:          ids.Event(),
				SessionID:   sid,
				Sequence:    int64(evtSeq),
				Ts:          now,
				Source:      "hook",
				Type:        "tool.pre_use",
				PayloadJSON: []byte(`{"tool_name":"Read"}`),
			}
			if err := db.InsertEventOne(dLocal, e); err != nil {
				t.Fatalf("InsertEventOne: %v", err)
			}
		}
		// Increment the tool_calls summary counter to match.
		for j := 0; j < ec.n; j++ {
			if err := db.IncrementSummaryCounter(dLocal, sid, "tool_calls"); err != nil {
				t.Fatalf("IncrementSummaryCounter: %v", err)
			}
		}
	}

	return dbPathLocal, dLocal, func() { dLocal.Close() }
}

// captureStatsStdout captures os.Stdout during fn, returning the output bytes.
func captureStatsStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	orig := os.Stdout
	t.Cleanup(func() { os.Stdout = orig })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	fn()

	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("captureStatsStdout copy: %v", err)
	}
	return buf.Bytes()
}

func TestRunStats_NoFilter(t *testing.T) {
	dbPath, _, cleanup := seedStatsDB(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	out := captureStatsStdout(t, func() {
		if err := runStats([]string{}); err != nil {
			t.Fatalf("runStats: %v", err)
		}
	})

	s := string(out)
	if !strings.Contains(s, "claude") {
		t.Errorf("output missing 'claude':\n%s", s)
	}
	if !strings.Contains(s, "codex") {
		t.Errorf("output missing 'codex':\n%s", s)
	}
	// Should show 2 claude sessions.
	if !strings.Contains(s, "2 sessions") {
		t.Errorf("output should contain '2 sessions' for claude/codex:\n%s", s)
	}
}

func TestRunStats_RepoFilter(t *testing.T) {
	dbPath, _, cleanup := seedStatsDB(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	out := captureStatsStdout(t, func() {
		if err := runStats([]string{"--repo", "/a"}); err != nil {
			t.Fatalf("runStats --repo: %v", err)
		}
	})

	s := string(out)
	// Under repo /a: 1 claude + 2 codex sessions.
	if !strings.Contains(s, "claude") {
		t.Errorf("output missing 'claude':\n%s", s)
	}
	if !strings.Contains(s, "codex") {
		t.Errorf("output missing 'codex':\n%s", s)
	}
	// scope line must mention the repo
	if !strings.Contains(s, "repo=/a") {
		t.Errorf("output missing 'repo=/a':\n%s", s)
	}
}

func TestRunStats_UserFilter(t *testing.T) {
	dbPath, _, cleanup := seedStatsDB(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	out := captureStatsStdout(t, func() {
		if err := runStats([]string{"--user", "alice"}); err != nil {
			t.Fatalf("runStats --user: %v", err)
		}
	})

	s := string(out)
	// alice has 2 claude sessions; bob's codex sessions should not appear as agent
	// under the user=alice filter. The "By agent" section should show claude only.
	if !strings.Contains(s, "user=alice") {
		t.Errorf("output missing 'user=alice':\n%s", s)
	}
	if !strings.Contains(s, "claude") {
		t.Errorf("output missing 'claude':\n%s", s)
	}
}

func TestRunStats_ErrUsage(t *testing.T) {
	err := runStats([]string{"unexpected_positional"})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage, got %v", err)
	}
}

func TestParseDurationWithDays(t *testing.T) {
	cases := []struct {
		input   string
		want    time.Duration
		wantErr bool
	}{
		{"7d", 7 * 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"30m", 30 * time.Minute, false},
		{"1d12h", 36 * time.Hour, false},
		{"invalid", 0, true},
	}
	for _, tc := range cases {
		got, err := parseDurationWithDays(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseDurationWithDays(%q): expected error, got %v", tc.input, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseDurationWithDays(%q): unexpected error: %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseDurationWithDays(%q): got %v, want %v", tc.input, got, tc.want)
		}
	}
}
