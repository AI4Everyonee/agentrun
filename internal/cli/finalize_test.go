package cli

import (
	"testing"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// TestRunFinalizeIdle_StaleSessionFlippedToCompleted verifies that running
// finalize-idle against a session with an old last event marks it as completed.
func TestRunFinalizeIdle_StaleSessionFlippedToCompleted(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	now := time.Now().UTC().Round(time.Microsecond)

	// Insert an old event (60 minutes ago).
	oldEvt := db.EventRow{
		ID:          "evt_stale",
		SessionID:   sessionID,
		Sequence:    1,
		Ts:          now.Add(-60 * time.Minute),
		Source:      "hook",
		Type:        "tool.pre_use",
		PayloadJSON: []byte(`{}`),
	}
	if err := db.InsertEventOne(d, oldEvt); err != nil {
		t.Fatalf("InsertEventOne: %v", err)
	}

	// Run finalize-idle with --older-than 30m.
	if err := runFinalizeIdle([]string{"--older-than", "30m"}); err != nil {
		t.Fatalf("runFinalizeIdle: %v", err)
	}

	// Assert status is now 'completed'.
	var status string
	if err := d.QueryRow(`SELECT status FROM session_summary WHERE session_id=?`, sessionID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "completed" {
		t.Errorf("status after finalize-idle: got %q, want 'completed'", status)
	}
}

// TestRunFinalizeIdle_RecentSessionUnaffected verifies that sessions with recent
// events are not touched by finalize-idle.
func TestRunFinalizeIdle_RecentSessionUnaffected(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	now := time.Now().UTC().Round(time.Microsecond)

	// Insert a recent event (1 minute ago).
	recentEvt := db.EventRow{
		ID:          "evt_recent2",
		SessionID:   sessionID,
		Sequence:    1,
		Ts:          now.Add(-1 * time.Minute),
		Source:      "hook",
		Type:        "tool.pre_use",
		PayloadJSON: []byte(`{}`),
	}
	if err := db.InsertEventOne(d, recentEvt); err != nil {
		t.Fatalf("InsertEventOne: %v", err)
	}

	// Run finalize-idle with default 30m threshold.
	if err := runFinalizeIdle([]string{}); err != nil {
		t.Fatalf("runFinalizeIdle: %v", err)
	}

	// Status must still be 'running'.
	var status string
	if err := d.QueryRow(`SELECT status FROM session_summary WHERE session_id=?`, sessionID).Scan(&status); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if status != "running" {
		t.Errorf("status after finalize-idle (should be unaffected): got %q, want 'running'", status)
	}
}

// TestRunFinalizeIdle_NoPositionalArgs verifies that extra positional args
// return ErrUsage.
func TestRunFinalizeIdle_NoPositionalArgs(t *testing.T) {
	err := runFinalizeIdle([]string{"extra_arg"})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage for extra positional arg, got %v", err)
	}
}

