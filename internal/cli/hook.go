package cli

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jeevan/agentrun/internal/db"
	"github.com/jeevan/agentrun/internal/gitmeta"
	"github.com/jeevan/agentrun/internal/hooks"
)

// defaultMaxPayloadBytes is the cap before truncation. Override via
// AGENTRUN_MAX_PAYLOAD_BYTES (integer, bytes).
const defaultMaxPayloadBytes = 256 * 1024

// runHook implements `agentrun hook <agent> <EventName>`.
//
// The agent argument is "claude" or "codex" — required so the recorder can
// populate sessions.agent for natively-started (unwrapped) sessions where
// the wrapper isn't present to set it.
//
// Contract:
//   - Reads a single JSON object from stdin (the hook payload from claude/codex).
//   - Persists one events row with source="hook" and a normalized type.
//   - For SessionStart on an unknown session, ensures a sessions row exists
//     (the wrapper used to do this; in the global-install model the hook is
//     the only entry point).
//   - For SessionEnd, marks the session completed (Claude only — Codex has
//     no documented SessionEnd event).
//   - ALWAYS returns nil (exit 0) on all paths — read-only observability never
//     blocks the parent agent.
//
// Session ID resolution (in order):
//  1. $AGENTRUN_SESSION_ID — set by the agentrun wrapper for opt-in PTY capture
//  2. Synthesized from the payload's session_id field as "s_native_<agent>_<uuid-without-dashes>"
//
// DB path resolution (in order):
//  1. $AGENTRUN_DB_PATH — set by the wrapper
//  2. $AGENTRUN_DB_DIR/agentrun.db
//  3. $HOME/.agentrun/agentrun.db
func runHook(args []string) error {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "agentrun hook: usage: agentrun hook <agent> <EventName>")
		return nil
	}
	agentName := args[0]
	if agentName != "claude" && agentName != "codex" {
		fmt.Fprintf(os.Stderr, "agentrun hook: unknown agent %q (expected claude or codex); ignoring\n", agentName)
		return nil
	}
	eventName := args[1]

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: read stdin: %v\n", err)
		return nil
	}

	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		raw = []byte(fmt.Sprintf(
			`{"agentrun_invalid_payload":true,"raw_b64":%q,"parse_error":%q,"hook_event_name":%q,"agent":%q}`,
			base64.StdEncoding.EncodeToString(raw), err.Error(), eventName, agentName,
		))
		probe = nil
	}

	maxBytes := defaultMaxPayloadBytes
	if v := os.Getenv("AGENTRUN_MAX_PAYLOAD_BYTES"); v != "" {
		if n, parseErr := strconv.Atoi(v); parseErr == nil && n > 0 {
			maxBytes = n
		}
	}
	if len(raw) > maxBytes {
		prefixLen := 4096
		if len(raw) < prefixLen {
			prefixLen = len(raw)
		}
		raw = []byte(fmt.Sprintf(
			`{"agentrun_truncated":true,"original_size":%d,"truncated_payload_prefix_b64":%q,"hook_event_name":%q,"agent":%q}`,
			len(raw), base64.StdEncoding.EncodeToString(raw[:prefixLen]), eventName, agentName,
		))
		probe = nil
	}

	sessionID, native := resolveSessionID(agentName, probe)
	if sessionID == "" {
		fmt.Fprintln(os.Stderr, "agentrun hook: cannot resolve session id (no AGENTRUN_SESSION_ID and no payload session_id); ignoring")
		return nil
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: cannot resolve db path: %v; ignoring\n", err)
		return nil
	}

	d, err := openForHook(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: open db: %v\n", err)
		return nil
	}
	defer d.Close()

	if native {
		if err := ensureNativeSessionRow(d, sessionID, agentName, probe); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: ensure session row: %v\n", err)
		}
	}

	m := hooks.Normalize(eventName)

	if _, err := db.InsertEventWithAutoSeq(
		d, sessionID, time.Now().UTC(), m.Source, m.Type, raw, "noop-1",
	); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: insert failed (event=%s session=%s): %v\n", eventName, sessionID, err)
		return nil
	}

	if eventName == "SessionStart" {
		var info struct {
			Model          string `json:"model"`
			TranscriptPath string `json:"transcript_path"`
		}
		if probeErr := json.Unmarshal(raw, &info); probeErr == nil {
			if info.Model != "" {
				if updateErr := db.UpdateSessionModel(d, sessionID, info.Model); updateErr != nil {
					fmt.Fprintf(os.Stderr, "agentrun hook: update model: %v\n", updateErr)
				}
			}
			if info.TranscriptPath != "" {
				if updateErr := db.UpdateSessionTranscriptPath(d, sessionID, info.TranscriptPath); updateErr != nil {
					fmt.Fprintf(os.Stderr, "agentrun hook: update transcript_path: %v\n", updateErr)
				}
			}
		}
	}

	// SessionEnd finalizes the row for native sessions. (For wrapper sessions,
	// the wrapper's recorder.Close finalizes — we still emit the event but skip
	// the status update to avoid racing with the wrapper.)
	if eventName == "SessionEnd" && native {
		if finalizeErr := db.FinalizeSession(
			d, sessionID, "", "completed", 0, time.Now().UTC(),
		); finalizeErr != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: finalize session: %v\n", finalizeErr)
		}
	}

	if m.Counter != "" {
		if incrErr := db.IncrementSummaryCounter(d, sessionID, m.Counter); incrErr != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: increment counter %q: %v\n", m.Counter, incrErr)
		}
	}

	return nil
}

// resolveSessionID returns (sessionID, native). When the wrapper has set
// AGENTRUN_SESSION_ID, native=false and we use that ID verbatim. Otherwise we
// synthesize an ID from the agent's own session_id field in the payload,
// signalling native=true so callers know to bootstrap the sessions row.
func resolveSessionID(agentName string, payload map[string]any) (string, bool) {
	if s := os.Getenv("AGENTRUN_SESSION_ID"); s != "" {
		return s, false
	}
	if payload == nil {
		return "", true
	}
	v, ok := payload["session_id"].(string)
	if !ok || v == "" {
		return "", true
	}
	clean := strings.ReplaceAll(v, "-", "")
	return fmt.Sprintf("s_native_%s_%s", agentName, clean), true
}

// resolveDBPath picks the database path with the same precedence as the
// wrapper's config.Load but evaluated inline so the hook command stays a
// single short-lived process with no extra package wiring.
func resolveDBPath() (string, error) {
	if p := os.Getenv("AGENTRUN_DB_PATH"); p != "" {
		return p, nil
	}
	if dir := os.Getenv("AGENTRUN_DB_DIR"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", fmt.Errorf("absolutize AGENTRUN_DB_DIR: %w", err)
		}
		return filepath.Join(abs, "agentrun.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("no AGENTRUN_DB_PATH/AGENTRUN_DB_DIR set and HOME unavailable: %w", err)
	}
	return filepath.Join(home, ".agentrun", "agentrun.db"), nil
}

// openForHook returns a *sql.DB suitable for one short hook invocation.
// If the DB file already exists, opens read-write WITHOUT running migrations
// (the wrapper or a prior hook already migrated). If it doesn't exist, opens
// via db.Open so the schema gets created — this covers the first-ever hook
// fire on a fresh install.
func openForHook(dbPath string) (*sql.DB, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if mkErr := os.MkdirAll(filepath.Dir(dbPath), 0o755); mkErr != nil {
				return nil, fmt.Errorf("mkdir db dir: %w", mkErr)
			}
			return db.Open(dbPath)
		}
		return nil, fmt.Errorf("stat db: %w", err)
	}
	return db.OpenReadWrite(dbPath)
}

// ensureNativeSessionRow creates the sessions + session_summary rows for a
// native (unwrapped) session if they don't already exist. Idempotent — safe
// to call on every hook fire; the existence check short-circuits.
func ensureNativeSessionRow(d *sql.DB, sessionID, agentName string, payload map[string]any) error {
	if _, err := db.GetSession(d, sessionID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup session: %w", err)
	}

	cwd := ""
	if payload != nil {
		if v, ok := payload["cwd"].(string); ok {
			cwd = v
		}
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	snap := gitmeta.Capture(cwd)
	nullStr := func(s string) sql.NullString {
		return sql.NullString{String: s, Valid: s != ""}
	}

	sess := db.SessionRow{
		ID:             sessionID,
		Agent:          agentName,
		Cwd:            cwd,
		RepoRoot:       nullStr(snap.RepoRoot),
		Branch:         nullStr(snap.Branch),
		StartCommitSHA: nullStr(snap.HeadSHA),
		StartedAt:      time.Now().UTC(),
		MetadataJSON:   `{"source":"native_hook"}`,
	}
	if err := db.InsertSession(d, sess); err != nil {
		// A peer hook subprocess may have raced us. Treat as success — the
		// other side won, and our event will still attach via FK.
		return nil
	}
	if err := db.InsertSessionSummary(d, sessionID, "running"); err != nil {
		return fmt.Errorf("insert summary: %w", err)
	}
	return nil
}
