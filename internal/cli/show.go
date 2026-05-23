package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
