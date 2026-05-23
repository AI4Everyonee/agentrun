package cli

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
)

// withStdout redirects os.Stdout to a pipe for the duration of fn, and returns
// all captured output.
func withStdout(t *testing.T, fn func()) []byte {
	t.Helper()
	origStdout := os.Stdout
	t.Cleanup(func() { os.Stdout = origStdout })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	fn()

	w.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, r); err != nil {
		t.Fatalf("copy stdout: %v", err)
	}
	return buf.Bytes()
}

// TestRunExport_EndToEnd builds a session with 3 events, runs runExport, and
// verifies the output has 1 header line + 3 event lines, each valid JSON, with
// payload as a parsed object (not a string).
func TestRunExport_EndToEnd(t *testing.T) {
	sessionID, dbPath, d, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	now := time.Now().UTC().Round(time.Microsecond)

	// Insert 3 events.
	for i := 1; i <= 3; i++ {
		payload := []byte(fmt.Sprintf(`{"turn_id":"turn-%d","seq":%d}`, i, i))
		e := db.EventRow{
			ID:               fmt.Sprintf("evt_exp%d", i),
			SessionID:        sessionID,
			Sequence:         int64(i),
			Ts:               now,
			Source:           "hook",
			Type:             "tool.pre_use",
			PayloadJSON:      payload,
			RedactionVersion: sql.NullString{String: "noop-1", Valid: true},
		}
		if err := db.InsertEventOne(d, e); err != nil {
			t.Fatalf("InsertEventOne %d: %v", i, err)
		}
	}

	var output []byte
	withStdoutCapture := func() {
		output = withStdout(t, func() {
			if err := runExport([]string{sessionID}); err != nil {
				t.Fatalf("runExport returned error: %v", err)
			}
		})
	}
	withStdoutCapture()

	// Split output into lines.
	scanner := bufio.NewScanner(bytes.NewReader(output))
	var lines []string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}

	// Expect 1 header line + 3 event lines = 4 lines total.
	if len(lines) != 4 {
		t.Fatalf("expected 4 lines (1 header + 3 events), got %d\noutput:\n%s", len(lines), output)
	}

	// Verify header line.
	var header map[string]json.RawMessage
	if err := json.Unmarshal([]byte(lines[0]), &header); err != nil {
		t.Fatalf("header line is not valid JSON: %v\nline: %s", err, lines[0])
	}
	if _, ok := header["agentrun_export_version"]; !ok {
		t.Errorf("header missing 'agentrun_export_version' field")
	}
	if _, ok := header["session"]; !ok {
		t.Errorf("header missing 'session' field")
	}
	if _, ok := header["artifacts"]; !ok {
		t.Errorf("header missing 'artifacts' field")
	}

	// Verify each event line.
	for i, line := range lines[1:] {
		var ev map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("event line %d is not valid JSON: %v\nline: %s", i+1, err, line)
		}

		// Must have required fields.
		for _, field := range []string{"id", "session_id", "sequence", "ts", "source", "type", "payload"} {
			if _, ok := ev[field]; !ok {
				t.Errorf("event line %d missing field %q", i+1, field)
			}
		}

		// payload must be a parsed object, not a string.
		var payload map[string]interface{}
		if err := json.Unmarshal(ev["payload"], &payload); err != nil {
			t.Errorf("event line %d: payload is not a JSON object: %v\nraw payload: %s", i+1, err, ev["payload"])
		}

		// Verify payload has the expected keys we inserted.
		if _, ok := payload["turn_id"]; !ok {
			t.Errorf("event line %d: payload missing 'turn_id'", i+1)
		}
	}
}

// TestRunExport_SessionNotFound verifies that export with an unknown session ID
// returns a descriptive error.
func TestRunExport_SessionNotFound(t *testing.T) {
	_, dbPath, _, cleanup := setupTestDB(t)
	defer cleanup()

	t.Setenv("AGENTRUN_DB_PATH", dbPath)

	err := runExport([]string{"s_does_not_exist"})
	if err == nil {
		t.Fatal("expected error for unknown session, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q should mention 'not found'", err.Error())
	}
}

// TestRunExport_MissingSessionID verifies that export with no args returns ErrUsage.
func TestRunExport_MissingSessionID(t *testing.T) {
	err := runExport([]string{})
	if err != ErrUsage {
		t.Errorf("expected ErrUsage, got %v", err)
	}
}
