package cli

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
)

// setupSearchDB creates a temp DB with one session and session_summary row.
// Returns (sessionID, dbPath, *sql.DB, cleanup).
func setupSearchDB(t *testing.T, agent string) (string, string, *sql.DB, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "search_test.db")

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	sid := "s_test_" + agent + "_" + strings.ReplaceAll(t.Name(), "/", "_")
	sess := db.SessionRow{
		ID:           sid,
		Agent:        agent,
		Cwd:          "/tmp/test",
		StartedAt:    time.Now().UTC(),
		MetadataJSON: "{}",
	}
	if err := db.InsertSession(d, sess); err != nil {
		d.Close()
		t.Fatalf("InsertSession: %v", err)
	}
	if err := db.InsertSessionSummary(d, sid, "completed"); err != nil {
		d.Close()
		t.Fatalf("InsertSessionSummary: %v", err)
	}
	return sid, dbPath, d, func() { d.Close() }
}

// insertEvent inserts a test event and returns its generated ID.
func insertEvent(t *testing.T, d *sql.DB, sessionID, eventType string, payload []byte) string {
	t.Helper()
	id, err := db.InsertEventWithAutoSeq(
		d, sessionID, time.Now().UTC(), "test", eventType, payload, "",
	)
	if err != nil {
		t.Fatalf("InsertEventWithAutoSeq(%s, %s): %v", sessionID, eventType, err)
	}
	return id
}

// TestSearch_FindsMatchingEvents seeds a session with 3 events:
//   - tool.pre_use  {"tool_name":"Bash","tool_input":{"command":"echo hello"}}
//   - user.prompt   {"prompt":"investigate the rate limit"}
//   - tool.pre_use  {"tool_name":"Edit"}
//
// Queries "rate limit" and expects exactly 1 hit (the user.prompt event).
func TestSearch_FindsMatchingEvents(t *testing.T) {
	sid, dbPath, d, cleanup := setupSearchDB(t, "claude")
	defer cleanup()

	insertEvent(t, d, sid, "tool.pre_use", []byte(`{"tool_name":"Bash","tool_input":{"command":"echo hello"}}`))
	insertEvent(t, d, sid, "user.prompt", []byte(`{"prompt":"investigate the rate limit"}`))
	insertEvent(t, d, sid, "tool.pre_use", []byte(`{"tool_name":"Edit"}`))

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	output := withStdout(t, func() {
		if err := runSearch([]string{"rate limit"}); err != nil {
			t.Fatalf("runSearch: %v", err)
		}
	})

	out := string(output)

	// Must contain "rate limit" (the matched snippet) and NOT "no matches".
	if strings.Contains(out, "no matches") {
		t.Fatalf("expected a hit but got 'no matches': %s", out)
	}
	if !strings.Contains(strings.ToLower(out), "rate") {
		t.Errorf("expected 'rate' in output, got:\n%s", out)
	}
	// Only one hit — the user.prompt event.
	if !strings.Contains(out, "user.prompt") {
		t.Errorf("expected user.prompt type in output, got:\n%s", out)
	}
	// Should NOT show tool.pre_use (those payloads don't mention rate limit).
	if strings.Contains(out, "tool.pre_use") {
		t.Errorf("expected no tool.pre_use hit for 'rate limit', got:\n%s", out)
	}
}

// TestSearch_SessionFilter seeds two sessions both containing "foo" and verifies
// that --session restricts results to only the specified session.
func TestSearch_SessionFilter(t *testing.T) {
	// Session A
	sidA, dbPathA, dA, cleanupA := setupSearchDB(t, "claude")
	defer cleanupA()

	insertEvent(t, dA, sidA, "user.prompt", []byte(`{"prompt":"foo bar baz"}`))
	dA.Close()

	// Reopen the same DB (both sessions go in the same file).
	// Actually, we need both sessions in the same DB for a single search call.
	// Create a fresh DB that has both sessions.
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "filter_test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	// Insert session A.
	sA := db.SessionRow{ID: "s_filter_A", Agent: "claude", Cwd: "/a", StartedAt: time.Now().UTC(), MetadataJSON: "{}"}
	if err := db.InsertSession(d, sA); err != nil {
		t.Fatalf("InsertSession A: %v", err)
	}
	if err := db.InsertSessionSummary(d, "s_filter_A", "completed"); err != nil {
		t.Fatalf("InsertSessionSummary A: %v", err)
	}
	insertEvent(t, d, "s_filter_A", "user.prompt", []byte(`{"prompt":"foo is in session A"}`))

	// Insert session B.
	sB := db.SessionRow{ID: "s_filter_B", Agent: "codex", Cwd: "/b", StartedAt: time.Now().UTC(), MetadataJSON: "{}"}
	if err := db.InsertSession(d, sB); err != nil {
		t.Fatalf("InsertSession B: %v", err)
	}
	if err := db.InsertSessionSummary(d, "s_filter_B", "completed"); err != nil {
		t.Fatalf("InsertSessionSummary B: %v", err)
	}
	insertEvent(t, d, "s_filter_B", "user.prompt", []byte(`{"prompt":"foo is in session B"}`))

	_ = dbPathA // not used for the combined test

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	output := withStdout(t, func() {
		if err := runSearch([]string{"foo", "--session", "s_filter_A"}); err != nil {
			t.Fatalf("runSearch: %v", err)
		}
	})

	out := string(output)

	if strings.Contains(out, "no matches") {
		t.Fatalf("expected a hit but got 'no matches': %s", out)
	}
	if !strings.Contains(out, "s_filter_A") {
		t.Errorf("expected s_filter_A in output, got:\n%s", out)
	}
	if strings.Contains(out, "s_filter_B") {
		t.Errorf("expected s_filter_B to be filtered out, got:\n%s", out)
	}
}

// TestSearch_TypeFilter seeds events of mixed types and verifies that --type
// restricts to only events whose type contains the substring.
func TestSearch_TypeFilter(t *testing.T) {
	sid, dbPath, d, cleanup := setupSearchDB(t, "claude")
	defer cleanup()

	insertEvent(t, d, sid, "user.prompt", []byte(`{"prompt":"typefilter hello world"}`))
	insertEvent(t, d, sid, "tool.pre_use", []byte(`{"tool_name":"Bash","typefilter":"hello world"}`))

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	output := withStdout(t, func() {
		if err := runSearch([]string{"--type", "user.prompt", "typefilter"}); err != nil {
			t.Fatalf("runSearch: %v", err)
		}
	})

	out := string(output)

	if strings.Contains(out, "no matches") {
		t.Fatalf("expected a hit but got 'no matches': %s", out)
	}
	if !strings.Contains(out, "user.prompt") {
		t.Errorf("expected user.prompt in output, got:\n%s", out)
	}
	// tool.pre_use should be filtered out by --type user.prompt.
	if strings.Contains(out, "tool.pre_use") {
		t.Errorf("expected tool.pre_use to be filtered out, got:\n%s", out)
	}
}

// TestSearch_NoResults verifies that a query with no matches prints "no matches"
// and returns nil (exit 0).
func TestSearch_NoResults(t *testing.T) {
	_, dbPath, _, cleanup := setupSearchDB(t, "claude")
	defer cleanup()

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	output := withStdout(t, func() {
		if err := runSearch([]string{"definitelynotinanypayload"}); err != nil {
			t.Fatalf("runSearch returned error: %v", err)
		}
	})

	out := string(output)
	if !strings.Contains(out, "no matches") {
		t.Errorf("expected 'no matches' in output, got: %s", out)
	}
}

// TestSearch_BadArgs verifies that an empty query (no positional args) returns ErrUsage.
func TestSearch_BadArgs(t *testing.T) {
	err := runSearch([]string{})
	if !errors.Is(err, ErrUsage) {
		t.Errorf("expected ErrUsage for empty args, got: %v", err)
	}
}
