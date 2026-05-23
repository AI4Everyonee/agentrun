package cli

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/jeevan/agentrun/internal/ids"
	"github.com/jeevan/agentrun/internal/redact"
	"github.com/jeevan/agentrun/internal/validation"
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

	redacted, redactionVersion := redact.Default().Redact(m.Type, raw)
	if _, err := db.InsertEventWithAutoSeq(
		d, sessionID, time.Now().UTC(), m.Source, m.Type, redacted, redactionVersion,
	); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: insert failed (event=%s session=%s): %v\n", eventName, sessionID, err)
		return nil
	}

	// Session enrichment: capture model/transcript_path on EVERY event that has
	// them (Codex includes model on most events; Claude includes neither on
	// SessionStart per our verification). UpdateSessionModel is a plain UPDATE
	// — cheap, idempotent, no harm in calling it repeatedly.
	enrichSession(d, sessionID, raw)

	// Validation classification: for PostToolUse on Bash, see if the command
	// matches a known test/build/lint/typecheck pattern. If so, emit a
	// validation.completed event and increment the session_summary counters.
	if eventName == "PostToolUse" {
		recordValidation(d, sessionID, probe)
	}

	// Native-session lifecycle:
	//   Stop          — turn boundary. Update ended_at + end_commit_sha so the
	//                   session reflects "last activity" while staying status=running
	//                   (long Claude sessions fire Stop after every turn; we mustn't
	//                   finalize prematurely).
	//   SessionEnd    — Claude only. Mark status='completed'. Codex sessions stay
	//                   status=running forever until `agentrun finalize-idle` runs.
	if native {
		switch eventName {
		case "Stop":
			markNativeStopActivity(d, sessionID, payloadCwd(probe))
		case "SessionEnd":
			cwd := payloadCwd(probe)
			endSHA := gitmeta.HeadCommit(cwd)
			if finalizeErr := db.FinalizeSession(
				d, sessionID, endSHA, "completed", 0, time.Now().UTC(),
			); finalizeErr != nil {
				fmt.Fprintf(os.Stderr, "agentrun hook: finalize session: %v\n", finalizeErr)
			}
			captureNativeGitDiff(d, sessionID, cwd, endSHA)
		}
	}

	if m.Counter != "" {
		if incrErr := db.IncrementSummaryCounter(d, sessionID, m.Counter); incrErr != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: increment counter %q: %v\n", m.Counter, incrErr)
		}
	}

	return nil
}

// enrichSession extracts model + transcript_path from the payload (best-effort)
// and updates the session row. UpdateSessionModel overwrites; UpdateSessionTranscriptPath
// is first-write-wins. Errors are logged and ignored — enrichment never blocks the hook.
func enrichSession(d *sql.DB, sessionID string, raw []byte) {
	var info struct {
		Model          string `json:"model"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return
	}
	if info.Model != "" {
		if err := db.UpdateSessionModel(d, sessionID, info.Model); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: update model: %v\n", err)
		}
	}
	if info.TranscriptPath != "" {
		if err := db.UpdateSessionTranscriptPath(d, sessionID, info.TranscriptPath); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: update transcript_path: %v\n", err)
		}
	}
}

// markNativeStopActivity bumps the session's ended_at to now (so "last activity"
// is queryable) and records end_commit_sha if it's still NULL. Status stays
// 'running' — that's only flipped to 'completed' by SessionEnd or by
// `agentrun finalize-idle`.
func markNativeStopActivity(d *sql.DB, sessionID, cwd string) {
	endSHA := gitmeta.HeadCommit(cwd)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := d.Exec(
		`UPDATE sessions
		    SET ended_at = ?,
		        end_commit_sha = COALESCE(end_commit_sha, NULLIF(?, ''))
		  WHERE id = ?`,
		now, endSHA, sessionID,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: mark stop activity: %v\n", err)
	}
}

// recordValidation classifies the Bash command in a PostToolUse payload, and
// if it matches a known test/build/lint/typecheck pattern, emits a
// `validation.completed` event and bumps the session_summary counters.
//
// Pass/fail is best-effort: we look for an exit_code field in `tool_response`
// (Claude) or `tool_result` (Codex). Absence is treated as pass — if the agent
// returned at all, the command didn't error out at the tool layer.
func recordValidation(d *sql.DB, sessionID string, probe map[string]any) {
	if probe == nil {
		return
	}
	toolName, _ := probe["tool_name"].(string)
	if toolName != "Bash" {
		return
	}
	toolInput, _ := probe["tool_input"].(map[string]any)
	if toolInput == nil {
		return
	}
	cmd, _ := toolInput["command"].(string)
	if cmd == "" {
		return
	}
	kind, ok := validation.Classify(cmd)
	if !ok {
		return
	}

	exitCode, hasExit := extractToolExitCode(probe)
	pass := !hasExit || exitCode == 0

	// Emit validation.completed event.
	payload := fmt.Sprintf(
		`{"kind":%q,"command":%q,"pass":%t,"exit_code":%d,"has_exit_code":%t}`,
		string(kind), cmd, pass, exitCode, hasExit,
	)
	if _, err := db.InsertEventWithAutoSeq(
		d, sessionID, time.Now().UTC(), "validation", "validation.completed",
		[]byte(payload), "noop-1",
	); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: insert validation event: %v\n", err)
	}

	// Bump counters.
	if err := db.IncrementSummaryCounter(d, sessionID, "validations_run"); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: bump validations_run: %v\n", err)
	}
	bumpCol := "validations_pass"
	if !pass {
		bumpCol = "validations_fail"
	}
	if err := db.IncrementSummaryCounter(d, sessionID, bumpCol); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: bump %s: %v\n", bumpCol, err)
	}
}

// extractToolExitCode peeks inside tool_response (Claude) or tool_result (Codex)
// for an exit_code field. Returns (0, false) if not found.
func extractToolExitCode(probe map[string]any) (int, bool) {
	for _, key := range []string{"tool_response", "tool_result"} {
		raw, ok := probe[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case map[string]any:
			if ec, ok := v["exit_code"]; ok {
				if f, ok := ec.(float64); ok {
					return int(f), true
				}
			}
		}
	}
	return 0, false
}

// payloadCwd returns payload["cwd"] as a string, or "" if not present.
// Used so gitmeta.HeadCommit runs in the session's cwd, not the hook process's.
func payloadCwd(probe map[string]any) string {
	if probe == nil {
		return ""
	}
	v, _ := probe["cwd"].(string)
	return v
}

// captureNativeGitDiff records the git diff between the session's start commit
// and the just-computed end commit as an artifact under <DB_DIR>/artifacts/<sid>/.
// Best-effort — any failure is logged and ignored.
//
// For native sessions, we don't have an in-process StartCommitSHA available;
// look it up from the sessions row instead. If no start commit exists (session
// wasn't in a git repo at start), we skip.
func captureNativeGitDiff(d *sql.DB, sessionID, cwd, endSHA string) {
	sess, err := db.GetSession(d, sessionID)
	if err != nil {
		return
	}
	if !sess.StartCommitSHA.Valid || sess.StartCommitSHA.String == "" {
		return
	}
	startSHA := sess.StartCommitSHA.String

	diff := gitmeta.Diff(cwd, startSHA, endSHA)
	if diff == "" {
		return
	}

	// Resolve artifacts dir without going through config.Load (avoids redundant
	// HOME lookups and works even if AGENTRUN_DB_DIR points elsewhere).
	dbPath, _ := resolveDBPath()
	artDir := filepath.Join(filepath.Dir(dbPath), "artifacts", sessionID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: mkdir artifacts dir: %v\n", err)
		return
	}
	path := filepath.Join(artDir, "git.diff")
	if err := os.WriteFile(path, []byte(diff), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: write git diff: %v\n", err)
		return
	}

	size, hash := fileSizeAndHashHex(path)
	stat := gitmeta.DiffStat(cwd, startSHA, endSHA)
	metaJSON := fmt.Sprintf(`{"stat":%q}`, stat)
	row := db.ArtifactRow{
		ID:           ids.Artifact(),
		SessionID:    sessionID,
		Kind:         "git_diff",
		Path:         sql.NullString{String: path, Valid: true},
		ContentHash:  sql.NullString{String: hash, Valid: true},
		SizeBytes:    sql.NullInt64{Int64: size, Valid: true},
		Mime:         sql.NullString{String: "text/x-diff", Valid: true},
		MetadataJSON: metaJSON,
	}
	if err := db.InsertArtifact(d, row); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: insert git_diff artifact: %v\n", err)
	}
}

// fileSizeAndHashHex returns (size, sha256-hex) of path. Returns (0, "") on any error.
func fileSizeAndHashHex(path string) (int64, string) {
	f, err := os.Open(path)
	if err != nil {
		return 0, ""
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, ""
	}
	return n, hex.EncodeToString(h.Sum(nil))
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
