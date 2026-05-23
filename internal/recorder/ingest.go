package recorder

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/gitmeta"
	"github.com/AI4Everyonee/agentrun/internal/hooks"
	"github.com/AI4Everyonee/agentrun/internal/ids"
	"github.com/AI4Everyonee/agentrun/internal/redact"
	"github.com/AI4Everyonee/agentrun/internal/userident"
	"github.com/AI4Everyonee/agentrun/internal/validation"
)

// defaultMaxPayloadBytes is the cap before truncation.
// Override via AGENTRUN_MAX_PAYLOAD_BYTES (integer, bytes).
const defaultIngestMaxPayloadBytes = 256 * 1024

// IngestRequest carries all the information needed to record one hook event.
// It is the canonical input to IngestEvent and is used by both the hook
// subcommand (direct path) and the collector server (socket path).
type IngestRequest struct {
	Agent     string          // "claude" | "codex"
	Event     string          // raw hook event name, e.g. "PreToolUse"
	SessionID string          // already-resolved session ID (may be "")
	Payload   json.RawMessage // the hook stdin JSON, raw
	Native    bool            // true if the wrapper didn't set AGENTRUN_SESSION_ID
}

// IngestEvent records one hook event into d. It applies the same
// validation/redaction/counter/lifecycle logic that runHook does directly.
// It always returns a descriptive error (the caller logs it and moves on;
// the parent agent is never blocked).
func IngestEvent(d *sql.DB, req IngestRequest) error {
	if req.Agent != "claude" && req.Agent != "codex" {
		return fmt.Errorf("IngestEvent: unknown agent %q", req.Agent)
	}

	raw := []byte(req.Payload)

	// Re-parse/validate the raw payload (may have come across the wire).
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		raw = []byte(fmt.Sprintf(
			`{"agentrun_invalid_payload":true,"raw_b64":%q,"parse_error":%q,"hook_event_name":%q,"agent":%q}`,
			base64.StdEncoding.EncodeToString(raw), err.Error(), req.Event, req.Agent,
		))
		probe = nil
	}

	maxBytes := defaultIngestMaxPayloadBytes
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
			len(raw), base64.StdEncoding.EncodeToString(raw[:prefixLen]), req.Event, req.Agent,
		))
		probe = nil
	}

	if req.SessionID == "" {
		return fmt.Errorf("IngestEvent: empty session id")
	}

	if req.Native {
		if err := ingestEnsureNativeSessionRow(d, req.SessionID, req.Agent, probe); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun collector: ensure session row: %v\n", err)
		}
	}

	m := hooks.Normalize(req.Event)

	redacted, redactionVersion := redact.Default().Redact(m.Type, raw)
	if _, err := db.InsertEventWithAutoSeq(
		d, req.SessionID, time.Now().UTC(), m.Source, m.Type, redacted, redactionVersion,
	); err != nil {
		return fmt.Errorf("IngestEvent: insert event: %w", err)
	}

	// Session enrichment.
	ingestEnrichSession(d, req.SessionID, raw)

	// Validation classification on PostToolUse.
	if req.Event == "PostToolUse" {
		ingestRecordValidation(d, req.SessionID, probe)
	}

	// Native-session lifecycle.
	if req.Native {
		switch req.Event {
		case "Stop":
			ingestMarkNativeStopActivity(d, req.SessionID, ingestPayloadCwd(probe))
		case "SessionEnd":
			cwd := ingestPayloadCwd(probe)
			endSHA := gitmeta.HeadCommit(cwd)
			if finalizeErr := db.FinalizeSession(
				d, req.SessionID, endSHA, "completed", 0, time.Now().UTC(),
			); finalizeErr != nil {
				fmt.Fprintf(os.Stderr, "agentrun collector: finalize session: %v\n", finalizeErr)
			}
			ingestCaptureNativeGitDiff(d, req.SessionID, cwd, endSHA)
		}
	}

	if m.Counter != "" {
		if incrErr := db.IncrementSummaryCounter(d, req.SessionID, m.Counter); incrErr != nil {
			fmt.Fprintf(os.Stderr, "agentrun collector: increment counter %q: %v\n", m.Counter, incrErr)
		}
	}

	return nil
}

// ingestEnrichSession extracts model + transcript_path from raw (best-effort).
func ingestEnrichSession(d *sql.DB, sessionID string, raw []byte) {
	var info struct {
		Model          string `json:"model"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return
	}
	if info.Model != "" {
		if err := db.UpdateSessionModel(d, sessionID, info.Model); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun collector: update model: %v\n", err)
		}
	}
	if info.TranscriptPath != "" {
		if err := db.UpdateSessionTranscriptPath(d, sessionID, info.TranscriptPath); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun collector: update transcript_path: %v\n", err)
		}
	}
}

// ingestMarkNativeStopActivity bumps ended_at without changing status.
func ingestMarkNativeStopActivity(d *sql.DB, sessionID, cwd string) {
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
		fmt.Fprintf(os.Stderr, "agentrun collector: mark stop activity: %v\n", err)
	}
}

// ingestRecordValidation classifies the Bash command in a PostToolUse payload.
func ingestRecordValidation(d *sql.DB, sessionID string, probe map[string]any) {
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

	exitCode, hasExit := ingestExtractToolExitCode(probe)
	pass := !hasExit || exitCode == 0

	payload := fmt.Sprintf(
		`{"kind":%q,"command":%q,"pass":%t,"exit_code":%d,"has_exit_code":%t}`,
		string(kind), cmd, pass, exitCode, hasExit,
	)
	if _, err := db.InsertEventWithAutoSeq(
		d, sessionID, time.Now().UTC(), "validation", "validation.completed",
		[]byte(payload), "noop-1",
	); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: insert validation event: %v\n", err)
	}

	if err := db.IncrementSummaryCounter(d, sessionID, "validations_run"); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: bump validations_run: %v\n", err)
	}
	bumpCol := "validations_pass"
	if !pass {
		bumpCol = "validations_fail"
	}
	if err := db.IncrementSummaryCounter(d, sessionID, bumpCol); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: bump %s: %v\n", bumpCol, err)
	}
}

// ingestExtractToolExitCode peeks inside tool_response/tool_result for exit_code.
func ingestExtractToolExitCode(probe map[string]any) (int, bool) {
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

// ingestPayloadCwd returns payload["cwd"] as a string, or "".
func ingestPayloadCwd(probe map[string]any) string {
	if probe == nil {
		return ""
	}
	v, _ := probe["cwd"].(string)
	return v
}

// ingestEnsureNativeSessionRow creates the sessions + session_summary rows for a
// native (unwrapped) session if they don't already exist.
func ingestEnsureNativeSessionRow(d *sql.DB, sessionID, agentName string, payload map[string]any) error {
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
		UserName:       nullStr(userident.Detect()),
	}
	if err := db.InsertSession(d, sess); err != nil {
		// A peer hook subprocess may have raced us.
		return nil
	}
	if err := db.InsertSessionSummary(d, sessionID, "running"); err != nil {
		return fmt.Errorf("insert summary: %w", err)
	}
	return nil
}

// ingestCaptureNativeGitDiff records the git diff as an artifact.
func ingestCaptureNativeGitDiff(d *sql.DB, sessionID, cwd, endSHA string) {
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

	// Resolve db path from environment (same logic as resolveDBPath in cli/hook.go).
	dbPath := ingestResolveDBPath()
	if dbPath == "" {
		return
	}
	artDir := filepath.Join(filepath.Dir(dbPath), "artifacts", sessionID)
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: mkdir artifacts dir: %v\n", err)
		return
	}
	path := filepath.Join(artDir, "git.diff")
	if err := os.WriteFile(path, []byte(diff), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: write git diff: %v\n", err)
		return
	}

	size, hash, hashErr := fileSizeAndHash(path)
	if hashErr != nil {
		return
	}
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
		fmt.Fprintf(os.Stderr, "agentrun collector: insert git_diff artifact: %v\n", err)
	}
}

// ingestResolveDBPath resolves the DB path from environment variables.
func ingestResolveDBPath() string {
	if p := os.Getenv("AGENTRUN_DB_PATH"); p != "" {
		return p
	}
	if dir := os.Getenv("AGENTRUN_DB_DIR"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return ""
		}
		return filepath.Join(abs, "agentrun.db")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".agentrun", "agentrun.db")
}
