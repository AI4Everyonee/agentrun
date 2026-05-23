package cli

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// runReplay re-emits captured PTY terminal.output events back through stdout,
// optionally in real time with --speed control.
// Usage: agentrun replay <session_id> [--speed N] [--no-realtime]
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	speed := fs.Float64("speed", 1.0, "playback speed multiplier (1.0 = real time)")
	noRealtime := fs.Bool("no-realtime", false, "dump entire stream as fast as possible")

	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	remaining := fs.Args()
	if len(remaining) != 1 {
		return ErrUsage
	}
	sid := remaining[0]

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	// Check that the session has a pty.raw artifact.
	artifacts, err := db.ListArtifacts(d, sid)
	if err != nil {
		return fmt.Errorf("replay: list artifacts: %w", err)
	}
	hasPTY := false
	for _, a := range artifacts {
		if a.Kind == "pty.raw" {
			hasPTY = true
			break
		}
	}
	if !hasPTY {
		fmt.Printf("session %s has no PTY capture (native sessions don't record terminal output)\n", sid)
		return nil
	}

	// Fetch all events for the session.
	events, err := db.FetchSessionEvents(d, sid)
	if err != nil {
		return fmt.Errorf("replay: fetch events: %w", err)
	}

	var prev *time.Time
	for _, ev := range events {
		if ev.Source != "pty" || ev.Type != "terminal.output" {
			continue
		}

		if !*noRealtime && prev != nil {
			delta := ev.Ts.Sub(*prev)
			if *speed > 0 {
				delta = time.Duration(float64(delta) / *speed)
			}
			// Cap gaps at 10 seconds to avoid very long pauses.
			if delta > 10*time.Second {
				delta = 10 * time.Second
			}
			if delta > 0 {
				time.Sleep(delta)
			}
		}

		raw := decodeBase64Payload(ev.PayloadJSON)
		if raw != nil {
			_, _ = os.Stdout.Write(raw)
		}

		ts := ev.Ts
		prev = &ts
	}

	return nil
}

// decodeBase64Payload extracts and base64-decodes the "bytes_b64" field from
// a terminal.output event payload JSON.
func decodeBase64Payload(payload []byte) []byte {
	var p struct {
		BytesB64 string `json:"bytes_b64"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil
	}
	raw, _ := base64.StdEncoding.DecodeString(p.BytesB64)
	return raw
}
