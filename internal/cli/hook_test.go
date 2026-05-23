package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jeevan/agentrun/internal/db"
	"github.com/jeevan/agentrun/internal/ids"
)

// setupTestDB creates a temp DB with a session and session_summary row for testing.
// Returns (sessionID, dbPath, *sql.DB, cleanup func()).
func setupTestDB(t *testing.T) (string, string, *sql.DB, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	sessionID := ids.Session()
	sess := db.SessionRow{
		ID:           sessionID,
		Agent:        "claude",
		AgentVersion: sql.NullString{String: "v1.0.0", Valid: true},
		Cwd:          "/tmp/test",
		StartedAt:    time.Now().UTC(),
		MetadataJSON: "{}",
	}
	if err := db.InsertSession(d, sess); err != nil {
		d.Close()
		t.Fatalf("InsertSession: %v", err)
	}
	if err := db.InsertSessionSummary(d, sessionID, "running"); err != nil {
		d.Close()
		t.Fatalf("InsertSessionSummary: %v", err)
	}

	cleanup := func() {
		d.Close()
	}
	return sessionID, dbPath, d, cleanup
}

// withStdin redirects os.Stdin to a pipe containing payload for the duration of fn.
func withStdin(t *testing.T, payload []byte, fn func()) {
	t.Helper()
	orig := os.Stdin
	t.Cleanup(func() { os.Stdin = orig })
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	go func() {
		_, _ = w.Write(payload)
		_ = w.Close()
	}()
	fn()
}

// countEventsForSession returns the number of events for the given session.
func countEventsForSession(t *testing.T, d *sql.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("countEventsForSession: %v", err)
	}
	return n
}

// getEventPayload returns the payload_json for the single event for a session.
func getEventPayload(t *testing.T, d *sql.DB, sessionID string) []byte {
	t.Helper()
	var payload []byte
	if err := d.QueryRow(`SELECT payload_json FROM events WHERE session_id=? LIMIT 1`, sessionID).Scan(&payload); err != nil {
		t.Fatalf("getEventPayload: %v", err)
	}
	return payload
}

// getEventType returns the type of the single event for a session.
func getEventType(t *testing.T, d *sql.DB, sessionID string) string {
	t.Helper()
	var typ string
	if err := d.QueryRow(`SELECT type FROM events WHERE session_id=? LIMIT 1`, sessionID).Scan(&typ); err != nil {
		t.Fatalf("getEventType: %v", err)
	}
	return typ
}

// getToolCallsCounter returns the tool_calls counter from session_summary.
func getToolCallsCounter(t *testing.T, d *sql.DB, sessionID string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT tool_calls FROM session_summary WHERE session_id=?`, sessionID).Scan(&n); err != nil {
		t.Fatalf("getToolCallsCounter: %v", err)
	}
	return n
}

// TestRunHook_HappyPath_PreToolUse verifies a PreToolUse hook call inserts a row
// with the correct type, source, and payload, and increments tool_calls.
func TestRunHook_HappyPath_PreToolUse(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`{"session_id":"abc","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`)

	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "PreToolUse"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	if n := countEventsForSession(t, d, sessionID); n != 1 {
		t.Fatalf("expected 1 event, got %d", n)
	}

	// Verify type and source.
	var evType, evSource string
	if err := d.QueryRow(`SELECT type, source FROM events WHERE session_id=?`, sessionID).Scan(&evType, &evSource); err != nil {
		t.Fatalf("scan event: %v", err)
	}
	if evType != "tool.pre_use" {
		t.Errorf("type: got %q, want %q", evType, "tool.pre_use")
	}
	if evSource != "hook" {
		t.Errorf("source: got %q, want %q", evSource, "hook")
	}

	// Verify payload round-trips.
	storedPayload := getEventPayload(t, d, sessionID)
	var original, stored map[string]any
	if err := json.Unmarshal(payload, &original); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(storedPayload, &stored); err != nil {
		t.Fatalf("unmarshal stored: %v", err)
	}
	// Verify tool_name key is present.
	if stored["tool_name"] != original["tool_name"] {
		t.Errorf("payload tool_name mismatch: got %v, want %v", stored["tool_name"], original["tool_name"])
	}

	// Verify counter incremented.
	if n := getToolCallsCounter(t, d, sessionID); n != 1 {
		t.Errorf("tool_calls: got %d, want 1", n)
	}
}

// TestRunHook_NoSessionIDAndNoPayload_Ignored verifies that when neither
// AGENTRUN_SESSION_ID is set nor the payload contains a session_id, the hook
// exits silently without inserting any rows.
func TestRunHook_NoSessionIDAndNoPayload_Ignored(t *testing.T) {
	_, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", "")
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`{"hook_event_name":"PreToolUse"}`) // no session_id field
	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "PreToolUse"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	var n int
	if qErr := d.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&n); qErr != nil {
		t.Fatalf("count: %v", qErr)
	}
	if n != 0 {
		t.Errorf("expected 0 events, got %d", n)
	}
}

// TestRunHook_NativeSession_CreatesRowFromPayload verifies that when
// AGENTRUN_SESSION_ID is unset but the payload includes a session_id, the
// hook synthesizes our session ID, creates the sessions+summary rows on
// the fly, and inserts the event against that new row.
func TestRunHook_NativeSession_CreatesRowFromPayload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	t.Setenv("AGENTRUN_SESSION_ID", "")
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	claudeUUID := "abcd1234-5678-9012-3456-7890abcdef01"
	payload := []byte(`{"session_id":"` + claudeUUID + `","cwd":"/tmp/native","hook_event_name":"SessionStart","model":"claude-sonnet-4-7","transcript_path":"/tmp/t.jsonl"}`)

	withStdin(t, payload, func() {
		if e := runHook([]string{"claude", "SessionStart"}); e != nil {
			t.Fatalf("runHook returned error: %v", e)
		}
	})

	wantID := "s_native_claude_" + strings.ReplaceAll(claudeUUID, "-", "")

	sess, err := db.GetSession(d, wantID)
	if err != nil {
		t.Fatalf("GetSession(%s): %v", wantID, err)
	}
	if sess.Agent != "claude" {
		t.Errorf("Agent = %q, want claude", sess.Agent)
	}
	if sess.Cwd != "/tmp/native" {
		t.Errorf("Cwd = %q, want /tmp/native", sess.Cwd)
	}
	if !sess.Model.Valid || sess.Model.String != "claude-sonnet-4-7" {
		t.Errorf("Model = %v, want claude-sonnet-4-7", sess.Model)
	}
	if !sess.TranscriptPath.Valid || sess.TranscriptPath.String != "/tmp/t.jsonl" {
		t.Errorf("TranscriptPath = %v, want /tmp/t.jsonl", sess.TranscriptPath)
	}

	if n := countEventsForSession(t, d, wantID); n != 1 {
		t.Errorf("expected 1 event for native session, got %d", n)
	}
}

// TestRunHook_NativeSession_SessionEndFinalizes verifies that for a native
// session, SessionEnd marks the session row as completed.
func TestRunHook_NativeSession_SessionEndFinalizes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	t.Setenv("AGENTRUN_SESSION_ID", "")
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	claudeUUID := "11111111-2222-3333-4444-555555555555"
	startPayload := []byte(`{"session_id":"` + claudeUUID + `","cwd":"/tmp/n","hook_event_name":"SessionStart"}`)
	endPayload := []byte(`{"session_id":"` + claudeUUID + `","cwd":"/tmp/n","hook_event_name":"SessionEnd"}`)

	withStdin(t, startPayload, func() {
		if e := runHook([]string{"claude", "SessionStart"}); e != nil {
			t.Fatalf("SessionStart: %v", e)
		}
	})
	withStdin(t, endPayload, func() {
		if e := runHook([]string{"claude", "SessionEnd"}); e != nil {
			t.Fatalf("SessionEnd: %v", e)
		}
	})

	wantID := "s_native_claude_" + strings.ReplaceAll(claudeUUID, "-", "")
	sess, err := db.GetSession(d, wantID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !sess.EndedAt.Valid {
		t.Error("EndedAt should be set after SessionEnd")
	}
	if !sess.ExitCode.Valid || sess.ExitCode.Int64 != 0 {
		t.Errorf("ExitCode = %v, want 0", sess.ExitCode)
	}

	var status string
	if qErr := d.QueryRow(`SELECT status FROM session_summary WHERE session_id=?`, wantID).Scan(&status); qErr != nil {
		t.Fatalf("scan summary status: %v", qErr)
	}
	if status != "completed" {
		t.Errorf("status = %q, want completed", status)
	}
}

// TestRunHook_NativeSession_StopUpdatesEndedAt verifies that Stop on a native
// session bumps ended_at (last activity) but does NOT flip status to completed —
// long Claude conversations fire Stop after every turn, so finalising on Stop
// would be premature.
func TestRunHook_NativeSession_StopUpdatesEndedAt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	t.Setenv("AGENTRUN_SESSION_ID", "")
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	claudeUUID := "deadbeef-0000-0000-0000-000000000000"
	startPayload := []byte(`{"session_id":"` + claudeUUID + `","cwd":"/tmp","hook_event_name":"SessionStart"}`)
	stopPayload := []byte(`{"session_id":"` + claudeUUID + `","cwd":"/tmp","hook_event_name":"Stop"}`)

	withStdin(t, startPayload, func() {
		if e := runHook([]string{"claude", "SessionStart"}); e != nil {
			t.Fatalf("SessionStart: %v", e)
		}
	})
	withStdin(t, stopPayload, func() {
		if e := runHook([]string{"claude", "Stop"}); e != nil {
			t.Fatalf("Stop: %v", e)
		}
	})

	wantID := "s_native_claude_" + strings.ReplaceAll(claudeUUID, "-", "")
	sess, err := db.GetSession(d, wantID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !sess.EndedAt.Valid {
		t.Error("EndedAt should be set after Stop")
	}

	var status string
	if qErr := d.QueryRow(`SELECT status FROM session_summary WHERE session_id=?`, wantID).Scan(&status); qErr != nil {
		t.Fatalf("scan status: %v", qErr)
	}
	if status != "running" {
		t.Errorf("status after Stop = %q, want running (Stop is per-turn, not session end)", status)
	}
}

// TestRunHook_NativeSession_ModelFromAnyEvent verifies that the model field is
// captured from any payload that includes it (Codex emits it on PreToolUse but
// not on SessionStart).
func TestRunHook_NativeSession_ModelFromAnyEvent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	t.Setenv("AGENTRUN_SESSION_ID", "")
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	uuid := "11112222-3333-4444-5555-666677778888"
	// SessionStart without model (typical Codex)
	startPayload := []byte(`{"session_id":"` + uuid + `","cwd":"/tmp","hook_event_name":"SessionStart"}`)
	// PreToolUse with model (Codex includes it here)
	pretoolPayload := []byte(`{"session_id":"` + uuid + `","cwd":"/tmp","hook_event_name":"PreToolUse","model":"gpt-5.5","tool_name":"Bash","tool_input":{"command":"ls"}}`)

	withStdin(t, startPayload, func() {
		if e := runHook([]string{"codex", "SessionStart"}); e != nil {
			t.Fatalf("SessionStart: %v", e)
		}
	})
	withStdin(t, pretoolPayload, func() {
		if e := runHook([]string{"codex", "PreToolUse"}); e != nil {
			t.Fatalf("PreToolUse: %v", e)
		}
	})

	wantID := "s_native_codex_" + strings.ReplaceAll(uuid, "-", "")
	sess, err := db.GetSession(d, wantID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !sess.Model.Valid || sess.Model.String != "gpt-5.5" {
		t.Errorf("Model = %v, want gpt-5.5 (captured from PreToolUse)", sess.Model)
	}
}

// TestRunHook_WrapperSession_SessionEndDoesNotFinalize verifies that when the
// wrapper sets AGENTRUN_SESSION_ID (native=false), SessionEnd inserts the
// event but does NOT touch the session row — the wrapper's recorder.Close
// owns finalization in that flow.
func TestRunHook_WrapperSession_SessionEndDoesNotFinalize(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`{"session_id":"x","cwd":"/","hook_event_name":"SessionEnd"}`)
	withStdin(t, payload, func() {
		if e := runHook([]string{"claude", "SessionEnd"}); e != nil {
			t.Fatalf("runHook: %v", e)
		}
	})

	sess, err := db.GetSession(d, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.EndedAt.Valid {
		t.Error("EndedAt should NOT be set by the hook for wrapper sessions; recorder.Close owns finalization")
	}
}

// TestRunHook_BadJSON_WrappedAndStored verifies that invalid JSON on stdin is
// wrapped into an error envelope and still persisted.
func TestRunHook_BadJSON_WrappedAndStored(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`not_valid_json`)
	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "PreToolUse"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	if n := countEventsForSession(t, d, sessionID); n != 1 {
		t.Fatalf("expected 1 event, got %d", n)
	}

	storedPayload := getEventPayload(t, d, sessionID)
	var m map[string]any
	if err := json.Unmarshal(storedPayload, &m); err != nil {
		t.Fatalf("stored payload is not valid JSON: %v", err)
	}
	if m["agentrun_invalid_payload"] != true {
		t.Errorf("expected agentrun_invalid_payload=true, got %v", m["agentrun_invalid_payload"])
	}
}

// TestRunHook_TruncatesLargePayload verifies that payloads exceeding 256 KiB
// are replaced with a truncation envelope.
func TestRunHook_TruncatesLargePayload(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	// Build a payload > 256 KiB.
	large := `{"x":"` + strings.Repeat("A", 300_000) + `"}`
	payload := []byte(large)

	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "PreToolUse"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	if n := countEventsForSession(t, d, sessionID); n != 1 {
		t.Fatalf("expected 1 event, got %d", n)
	}

	storedPayload := getEventPayload(t, d, sessionID)
	var m map[string]any
	if err := json.Unmarshal(storedPayload, &m); err != nil {
		t.Fatalf("stored payload is not valid JSON: %v\nraw: %s", err, storedPayload)
	}
	if m["agentrun_truncated"] != true {
		t.Errorf("expected agentrun_truncated=true, got %v", m["agentrun_truncated"])
	}
	origSize, ok := m["original_size"].(float64)
	if !ok {
		t.Fatalf("original_size missing or wrong type: %v", m["original_size"])
	}
	if origSize <= float64(256*1024) {
		t.Errorf("original_size should be >256KiB, got %v", origSize)
	}
}

// TestRunHook_SessionStart_UpdatesSessionRow verifies that a SessionStart hook
// call updates the model and transcript_path in the sessions table.
func TestRunHook_SessionStart_UpdatesSessionRow(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`{"session_id":"x","cwd":"/","hook_event_name":"SessionStart","model":"claude-sonnet-4-7","transcript_path":"/tmp/t.jsonl"}`)

	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "SessionStart"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	sess, err := db.GetSession(d, sessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !sess.Model.Valid || sess.Model.String != "claude-sonnet-4-7" {
		t.Errorf("Model: got %v, want %q", sess.Model, "claude-sonnet-4-7")
	}
	if !sess.TranscriptPath.Valid || sess.TranscriptPath.String != "/tmp/t.jsonl" {
		t.Errorf("TranscriptPath: got %v, want %q", sess.TranscriptPath, "/tmp/t.jsonl")
	}
}

// TestRunHook_UnknownEvent_StoredVerbatim verifies that an unknown event is
// stored with type="hook.<EventName>".
func TestRunHook_UnknownEvent_StoredVerbatim(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`{"hook_event_name":"UnknownExperimentalEvent","whatever":1}`)

	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "UnknownExperimentalEvent"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	if n := countEventsForSession(t, d, sessionID); n != 1 {
		t.Fatalf("expected 1 event, got %d", n)
	}

	evType := getEventType(t, d, sessionID)
	if evType != "hook.UnknownExperimentalEvent" {
		t.Errorf("type: got %q, want %q", evType, "hook.UnknownExperimentalEvent")
	}

	// Verify source is "hook".
	var evSource string
	if err := d.QueryRow(`SELECT source FROM events WHERE session_id=?`, sessionID).Scan(&evSource); err != nil {
		t.Fatalf("scan source: %v", err)
	}
	if evSource != "hook" {
		t.Errorf("source: got %q, want %q", evSource, "hook")
	}
}

// TestRunHook_BadArgs_ReturnsErrUsage verifies that calling runHook with no
// event name logs to stderr and returns nil (exit 0).
func TestRunHook_BadArgs_ReturnsErrUsage(t *testing.T) {
	// Per the plan §5.3 step 1: wrong argv logs to stderr and returns nil.
	// (The spec originally said ErrUsage but §5.3 step 1 says return nil.)
	err := runHook([]string{})
	// The plan step 1 says: write to stderr and return nil (exit 0).
	// We verify it does NOT return ErrUsage (return nil).
	if errors.Is(err, ErrUsage) {
		t.Error("runHook with no args should return nil, not ErrUsage")
	}
	if err != nil {
		t.Errorf("runHook with no args returned unexpected error: %v", err)
	}
}

// TestRunHook_NonExistentDB_NoPanic verifies that a missing DB path results in
// a silent log-and-continue (nil error, no panic).
func TestRunHook_NonExistentDB_NoPanic(t *testing.T) {
	sessionID, _, _, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", "/nonexistent/path/agentrun.db")

	payload := []byte(`{"hook_event_name":"PreToolUse"}`)
	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "PreToolUse"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}
}

// TestRunHook_CounterIncrement_Notification verifies that a Notification event
// (which has no counter) leaves all session_summary counters at 0.
func TestRunHook_CounterIncrement_Notification(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_SESSION_ID", sessionID)
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	payload := []byte(`{"hook_event_name":"Notification","message":"hello"}`)

	var err error
	withStdin(t, payload, func() {
		err = runHook([]string{"claude", "Notification"})
	})

	if err != nil {
		t.Fatalf("runHook returned error: %v", err)
	}

	// All counters should still be 0.
	var userPrompts, toolCalls, errs, appReq, appDen int
	row := d.QueryRow(`SELECT user_prompts, tool_calls, errors, approvals_request, approvals_denied FROM session_summary WHERE session_id=?`, sessionID)
	if err := row.Scan(&userPrompts, &toolCalls, &errs, &appReq, &appDen); err != nil {
		t.Fatalf("scan counters: %v", err)
	}
	if userPrompts != 0 || toolCalls != 0 || errs != 0 || appReq != 0 || appDen != 0 {
		t.Errorf("expected all counters=0 after Notification, got prompts=%d calls=%d errs=%d req=%d den=%d",
			userPrompts, toolCalls, errs, appReq, appDen)
	}
}
