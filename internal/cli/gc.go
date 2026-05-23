package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/config"
	"github.com/AI4Everyonee/agentrun/internal/db"
)

// runGC implements `agentrun gc [--older-than <duration>] [--dry-run]`.
//
// It deletes sessions (and their cascading events/artifacts) that are:
//   - older than --older-than (default: 30d)
//   - not currently in 'running' status
//
// With --dry-run it only reports what would be deleted.
func runGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	olderThanStr := fs.String("older-than", "30d", "delete sessions older than this (e.g. 30d, 7d, 1h, 30m)")
	dryRun := fs.Bool("dry-run", false, "print what would be deleted without actually deleting")

	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if len(fs.Args()) > 0 {
		return ErrUsage
	}

	olderThan, err := parseDurationWithDays(*olderThanStr)
	if err != nil {
		return fmt.Errorf("agentrun gc: invalid --older-than %q: %w", *olderThanStr, err)
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

	cutoff := time.Now().UTC().Add(-olderThan)

	if *dryRun {
		n, err := countSessionsToGC(d, cutoff)
		if err != nil {
			return fmt.Errorf("agentrun gc: %w", err)
		}
		fmt.Printf("agentrun: would delete %d sessions older than %s\n", n, *olderThanStr)
		return nil
	}

	// Real delete path — CASCADE removes events, artifacts, session_summary.
	ids, err := db.DeleteSessionsOlderThan(d, cutoff)
	if err != nil {
		return fmt.Errorf("agentrun gc: %w", err)
	}

	// Remove on-disk artifact directories.
	for _, sid := range ids {
		artDir := filepath.Join(cfg.ArtifactsDir, sid)
		if err := os.RemoveAll(artDir); err != nil {
			// Log but don't fail — DB rows are already gone.
			fmt.Fprintf(os.Stderr, "agentrun gc: remove artifacts dir %s: %v\n", artDir, err)
		}
	}

	fmt.Printf("agentrun: deleted %d sessions older than %s\n", len(ids), *olderThanStr)
	return nil
}

// countSessionsToGC counts sessions that would be removed by gc without deleting them.
func countSessionsToGC(d *sql.DB, cutoff time.Time) (int, error) {
	cutoffStr := cutoff.UTC().Format(time.RFC3339Nano)
	const q = `
		SELECT COUNT(*)
		FROM sessions s
		JOIN session_summary ss ON ss.session_id = s.id
		WHERE s.started_at < ?
		  AND ss.status != 'running'`

	var n int
	if err := d.QueryRow(q, cutoffStr).Scan(&n); err != nil {
		return 0, fmt.Errorf("countSessionsToGC: %w", err)
	}
	return n, nil
}

// parseDurationWithDays extends time.ParseDuration to accept a trailing "d"
// suffix for days. The "d" is treated as 24h. Compound expressions like "1d12h"
// are supported: days are parsed first, then the remainder is passed to
// time.ParseDuration.
//
// Examples:
//
//	"30d"   → 720h0m0s
//	"7d"    → 168h0m0s
//	"1d12h" → 36h0m0s
//	"1h"    → 1h0m0s  (stdlib)
//	"30m"   → 30m0s   (stdlib)
func parseDurationWithDays(s string) (time.Duration, error) {
	if !strings.ContainsRune(s, 'd') {
		return time.ParseDuration(s)
	}

	// Split on the first 'd': "30d" → ["30",""], "1d12h" → ["1","12h"]
	parts := strings.SplitN(s, "d", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("parseDurationWithDays: unexpected format %q", s)
	}

	daysStr := strings.TrimSpace(parts[0])
	remainder := strings.TrimSpace(parts[1])

	days, err := strconv.Atoi(daysStr)
	if err != nil || days < 0 {
		return 0, fmt.Errorf("parseDurationWithDays: invalid day count in %q", s)
	}

	total := time.Duration(days) * 24 * time.Hour

	if remainder != "" {
		rest, err := time.ParseDuration(remainder)
		if err != nil {
			return 0, fmt.Errorf("parseDurationWithDays: invalid remainder %q in %q: %w", remainder, s, err)
		}
		total += rest
	}

	return total, nil
}
