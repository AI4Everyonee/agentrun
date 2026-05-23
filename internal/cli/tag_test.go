package cli

import (
	"strings"
	"testing"
)

func TestRunTag_HappyPath(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	out := captureStdout(t, func() {
		if err := runTag([]string{sessionID, "important"}); err != nil {
			t.Fatalf("runTag: %v", err)
		}
	})

	// Output should mention the session and tag.
	s := string(out)
	if !strings.Contains(s, "important") {
		t.Errorf("output missing tag 'important':\n%s", s)
	}
	if !strings.Contains(s, sessionID) {
		t.Errorf("output missing session ID:\n%s", s)
	}

	// Verify an event row exists with type=user.tag and tags=important.
	var evType, tags string
	var payloadJSON []byte
	err := d.QueryRow(
		`SELECT type, COALESCE(tags,''), payload_json FROM events WHERE session_id=? AND type='user.tag' LIMIT 1`,
		sessionID,
	).Scan(&evType, &tags, &payloadJSON)
	if err != nil {
		t.Fatalf("query user.tag event: %v", err)
	}
	if evType != "user.tag" {
		t.Errorf("event type: got %q, want 'user.tag'", evType)
	}
	if tags != "important" {
		t.Errorf("event tags: got %q, want 'important'", tags)
	}
	if !strings.Contains(string(payloadJSON), "important") {
		t.Errorf("payload_json missing 'important': %s", payloadJSON)
	}
}

func TestRunTag_NonExistentSession(t *testing.T) {
	_, dbPath, _, cleanup := setupTestDB(t)
	defer cleanup()
	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	err := runTag([]string{"s_does_not_exist_xyz", "mytag"})
	if err == nil {
		t.Fatal("expected error for non-existent session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

func TestRunTag_EmptyTag(t *testing.T) {
	err := runTag([]string{"some_session_id", ""})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage for empty tag, got %v", err)
	}
}

func TestRunTag_ErrUsage_WrongArgs(t *testing.T) {
	// Only one arg.
	err := runTag([]string{"only_one"})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage for single arg, got %v", err)
	}
	// Zero args.
	err = runTag([]string{})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage for zero args, got %v", err)
	}
}
