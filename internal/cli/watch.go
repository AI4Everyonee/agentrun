package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jeevan/agentrun/internal/config"
	"github.com/jeevan/agentrun/internal/db"
)

// runWatch implements `agentrun watch [--filter <type>] [--since <duration>]`.
//
// It tails the events table in real time by polling every 250ms, printing each
// new event in a fixed-width human-readable format. Press Ctrl+C to exit.
func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	filterType := fs.String("filter", "", "only show events whose type contains this substring")
	sinceStr := fs.String("since", "0s", "start from events this far in the past (e.g. 1h, 30m)")

	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if len(fs.Args()) > 0 {
		return ErrUsage
	}

	since, err := parseDurationWithDays(*sinceStr)
	if err != nil {
		return fmt.Errorf("agentrun watch: invalid --since %q: %w", *sinceStr, err)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	// Install signal handler for clean Ctrl+C exit.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	poll := 250 * time.Millisecond
	lastTs := time.Now().UTC().Add(-since)

	fmt.Fprintf(os.Stderr, "agentrun watch: tailing events (Ctrl+C to stop)...\n")

	for {
		select {
		case <-stop:
			fmt.Fprintln(os.Stderr, "\nagentrun watch: stopped.")
			return nil
		default:
		}

		rows, err := db.TailEventsSince(d, lastTs, 100)
		if err != nil {
			fmt.Fprintf(os.Stderr, "agentrun watch: query error: %v\n", err)
		} else {
			for _, ev := range rows {
				if *filterType != "" && !strings.Contains(ev.Type, *filterType) {
					if ev.Ts.After(lastTs) {
						lastTs = ev.Ts
					}
					continue
				}
				printWatchEvent(ev)
				if ev.Ts.After(lastTs) {
					lastTs = ev.Ts
				}
			}
		}

		// Sleep with interruptible select.
		select {
		case <-stop:
			fmt.Fprintln(os.Stderr, "\nagentrun watch: stopped.")
			return nil
		case <-time.After(poll):
		}
	}
}

// printWatchEvent formats one event row to stdout.
//
// Format:
//
//	2026-05-23 13:42:01.123  s_native_claude_abc12345  hook   tool.pre_use         Bash: echo hello
//
// Columns:
//   - Local timestamp (datetime, ms precision)
//   - Session ID (30 chars)
//   - Source (6 chars)
//   - Type (20 chars)
//   - Hint (60 chars): tool_name + tool_input.command, or prompt excerpt, or blank
func printWatchEvent(ev db.EventRow) {
	ts := ev.Ts.Local().Format("2006-01-02 15:04:05.000")
	sid := truncate(ev.SessionID, 30)
	source := truncate(ev.Source, 6)
	evType := truncate(ev.Type, 20)
	hint := watchHint(ev.PayloadJSON)

	fmt.Printf("%-23s  %-30s  %-6s  %-20s  %s\n", ts, sid, source, evType, hint)
}

// watchHint extracts a short human-readable string from a raw event payload.
// Priority:
//  1. tool_name + tool_input.command → "<tool>: <command>"
//  2. prompt field → prompt excerpt
//  3. blank
func watchHint(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}

	// Try tool_name + tool_input.command.
	if raw, ok := m["tool_name"]; ok {
		var toolName string
		if err := json.Unmarshal(raw, &toolName); err == nil && toolName != "" {
			var cmd string
			if inputRaw, ok := m["tool_input"]; ok {
				var toolInput map[string]json.RawMessage
				if err := json.Unmarshal(inputRaw, &toolInput); err == nil {
					if cmdRaw, ok := toolInput["command"]; ok {
						_ = json.Unmarshal(cmdRaw, &cmd)
					}
				}
			}
			hint := toolName
			if cmd != "" {
				// Collapse newlines and trim.
				cmd = strings.ReplaceAll(cmd, "\n", " ")
				cmd = strings.TrimSpace(cmd)
				hint = toolName + ": " + cmd
			}
			return truncate(hint, 60)
		}
	}

	// Try prompt field.
	if raw, ok := m["prompt"]; ok {
		var prompt string
		if err := json.Unmarshal(raw, &prompt); err == nil && prompt != "" {
			prompt = strings.ReplaceAll(prompt, "\n", " ")
			return truncate(strings.TrimSpace(prompt), 60)
		}
	}

	return ""
}
