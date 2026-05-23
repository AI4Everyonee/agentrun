package cli

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/jeevan/agentrun/internal/db"
	"github.com/jeevan/agentrun/internal/hooks"
)

// defaultMaxPayloadBytes is the cap before truncation. Override via
// AGENTRUN_MAX_PAYLOAD_BYTES (integer, bytes).
const defaultMaxPayloadBytes = 256 * 1024

// runHook implements the `agentrun hook <EventName>` subcommand.
//
// Contract:
//   - args is the slice AFTER "hook" (so args[0] should be the event name).
//   - Reads a single JSON object from stdin (Claude delivers one per invocation).
//   - Persists one events row with source="hook" and a normalized type.
//   - ALWAYS exits 0 on the happy path AND on the orphan path. Returns nil so
//     root.go does not print a "agentrun: ..." error.
//   - Returns a non-nil error ONLY for a truly catastrophic, unrecoverable
//     issue (e.g. cli.ErrUsage if argv shape is wrong). main() will exit 1 in
//     that case; Claude treats non-zero as a non-blocking hook error.
//
// NB: We accept a non-zero exit ONLY for malformed argv (cli.ErrUsage path).
// All other failure modes log to stderr and return nil → exit 0.
func runHook(args []string) error {
	// Step 1: Argv check.
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "agentrun hook: usage: agentrun hook <EventName>")
		return nil
	}

	// Step 2: Event name capture.
	argEventName := args[0]

	// Step 3: Env check.
	sessionID := os.Getenv("AGENTRUN_SESSION_ID")
	dbPath := os.Getenv("AGENTRUN_DB_PATH")
	if sessionID == "" || dbPath == "" {
		fmt.Fprintln(os.Stderr, "agentrun hook: orphan invocation (AGENTRUN_SESSION_ID or AGENTRUN_DB_PATH unset); ignoring")
		return nil
	}

	// Step 4: Read stdin.
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: read stdin: %v\n", err)
		return nil
	}

	// Step 5: Validate JSON.
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		raw = []byte(fmt.Sprintf(`{"agentrun_invalid_payload":true,"raw_b64":%q,"parse_error":%q}`,
			base64.StdEncoding.EncodeToString(raw), err.Error()))
	}

	// Step 6: Truncate if too big.
	maxBytes := defaultMaxPayloadBytes
	if v := os.Getenv("AGENTRUN_MAX_PAYLOAD_BYTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			maxBytes = n
		}
	}
	if len(raw) > maxBytes {
		prefix := raw[:4096]
		raw = []byte(fmt.Sprintf(`{"agentrun_truncated":true,"original_size":%d,"truncated_payload_prefix_b64":%q,"hook_event_name":%q}`,
			len(raw), base64.StdEncoding.EncodeToString(prefix), argEventName))
	}

	// Step 7: Resolve mapping.
	m := hooks.Normalize(argEventName)

	// Step 8: Open DB read-write WITHOUT migrating.
	d, err := db.OpenReadWrite(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: open db: %v\n", err)
		return nil
	}
	defer d.Close()

	// Step 9: Insert event with auto-sequence + retry.
	_, err = db.InsertEventWithAutoSeq(d, sessionID, time.Now().UTC(), m.Source, m.Type, raw, "noop-1")
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun hook: insert failed (event=%s session=%s): %v\n", argEventName, sessionID, err)
		return nil
	}

	// Step 10: Optional session enrichment (SessionStart only).
	if argEventName == "SessionStart" {
		var info struct {
			Model          string `json:"model"`
			TranscriptPath string `json:"transcript_path"`
		}
		if jsonErr := json.Unmarshal(raw, &info); jsonErr == nil {
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

	// Step 12: Counter increment.
	if m.Counter != "" {
		if incrErr := db.IncrementSummaryCounter(d, sessionID, m.Counter); incrErr != nil {
			fmt.Fprintf(os.Stderr, "agentrun hook: increment counter %q: %v\n", m.Counter, incrErr)
		}
	}

	// Step 13: Return nil. Exit 0.
	return nil
}
