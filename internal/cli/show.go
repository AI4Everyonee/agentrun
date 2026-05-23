package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jeevan/agentrun/internal/config"
	"github.com/jeevan/agentrun/internal/db"
)

// runShow prints a detailed summary for a single session.
func runShow(args []string) error {
	if len(args) != 1 {
		return ErrUsage
	}
	sessionID := args[0]

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	s, err := db.GetSession(d, sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %q not found", sessionID)
		}
		return err
	}

	counts, err := db.CountEventsByType(d, sessionID)
	if err != nil {
		return err
	}

	artifacts, err := db.ListArtifacts(d, sessionID)
	if err != nil {
		return err
	}

	events, err := db.FetchSessionEvents(d, sessionID)
	if err != nil {
		return err
	}

	// --- Header block ---
	fmt.Printf("Session: %s\n", s.ID)
	if s.AgentVersion.Valid {
		fmt.Printf("Agent: %s (%s)\n", s.Agent, s.AgentVersion.String)
	} else {
		fmt.Printf("Agent: %s\n", s.Agent)
	}
	if s.RepoRoot.Valid {
		fmt.Printf("Repo: %s\n", s.RepoRoot.String)
	}
	fmt.Printf("Cwd: %s\n", s.Cwd)
	if s.Branch.Valid {
		fmt.Printf("Branch: %s\n", s.Branch.String)
	}
	if s.StartCommitSHA.Valid {
		fmt.Printf("Start commit: %s\n", shortSHA(s.StartCommitSHA.String))
	}
	if s.EndCommitSHA.Valid {
		fmt.Printf("End commit:   %s\n", shortSHA(s.EndCommitSHA.String))
	} else {
		fmt.Printf("End commit:   (none)\n")
	}
	fmt.Printf("Started: %s\n", s.StartedAt.UTC().Format(time.RFC3339))
	if s.EndedAt.Valid {
		fmt.Printf("Ended:   %s\n", s.EndedAt.Time.UTC().Format(time.RFC3339))
	}
	if s.ExitCode.Valid {
		fmt.Printf("Exit code: %d\n", s.ExitCode.Int64)
	}

	// --- Events block ---
	fmt.Println()
	fmt.Println("Events:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t: %d\n", k, counts[k])
	}
	tw.Flush()

	// --- Turns block (only for sessions that have turn_id in payloads) ---
	turns := buildTurns(events)
	if len(turns) > 0 {
		fmt.Println()
		fmt.Println("Turns:")
		const maxTurns = 10
		displayed := turns
		extra := 0
		if len(turns) > maxTurns {
			displayed = turns[:maxTurns]
			extra = len(turns) - maxTurns
		}
		for _, ti := range displayed {
			fmt.Printf("  %s: %s\n", ti.ID, strings.Join(ti.EventTypes, ", "))
		}
		if extra > 0 {
			fmt.Printf("  ... (%d more)\n", extra)
		}
	}

	// --- Artifacts block ---
	fmt.Println()
	fmt.Println("Artifacts:")
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, a := range artifacts {
		path := a.Path.String
		sizeStr := "(empty)"
		if a.SizeBytes.Valid && a.SizeBytes.Int64 > 0 {
			sizeStr = humanBytes(a.SizeBytes.Int64)
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", a.Kind, filepath.Base(path), sizeStr)
	}
	tw.Flush()

	return nil
}

// shortSHA returns the first 8 characters of a commit SHA, or the full string
// if it is shorter than 8 characters.
func shortSHA(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// turnInfo groups event types seen within a single turn_id.
type turnInfo struct {
	ID         string
	EventTypes []string // ordered by first occurrence, deduped
}

// buildTurns groups events by turn_id extracted from payload JSON.
// Events without a turn_id are ignored. Returns turns in first-seen order.
func buildTurns(events []db.EventRow) []turnInfo {
	var order []string // turn_id first-seen order
	seen := make(map[string]bool)

	for _, e := range events {
		var payload map[string]interface{}
		if err := json.Unmarshal(e.PayloadJSON, &payload); err != nil {
			continue
		}
		turnID, _ := payload["turn_id"].(string)
		if turnID == "" {
			continue
		}
		if !seen[turnID] {
			seen[turnID] = true
			order = append(order, turnID)
		}
	}

	result := make([]turnInfo, 0, len(order))
	for _, tid := range order {
		// Preserve the event type order as seen in sequence (deduped).
		var types []string
		typeSeen := make(map[string]bool)
		for _, e := range events {
			var payload map[string]interface{}
			if err := json.Unmarshal(e.PayloadJSON, &payload); err != nil {
				continue
			}
			if tid2, _ := payload["turn_id"].(string); tid2 != tid {
				continue
			}
			if !typeSeen[e.Type] {
				typeSeen[e.Type] = true
				types = append(types, e.Type)
			}
		}
		result = append(result, turnInfo{ID: tid, EventTypes: types})
	}
	return result
}

// humanBytes formats n bytes as a human-readable string (B / KB / MB / GB).
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n < k:
		return fmt.Sprintf("%d B", n)
	case n < k*k:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(k))
	case n < k*k*k:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(k*k))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(k*k*k))
	}
}
