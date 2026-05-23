package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// runStats prints per-agent and per-repo usage rollups.
// Usage: agentrun stats [--repo <dir>] [--user <name>] [--since <duration>]
func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	repo := fs.String("repo", "", "filter to a single repo root directory")
	user := fs.String("user", "", "filter to a single user_name")
	since := fs.String("since", "", "only count sessions started in the last N (e.g. 7d, 24h, 30m)")

	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if len(fs.Args()) > 0 {
		return ErrUsage
	}

	var sinceTime time.Time
	if *since != "" {
		d, err := parseDurationWithDays(*since)
		if err != nil {
			return fmt.Errorf("stats: invalid --since value %q: %w", *since, err)
		}
		sinceTime = time.Now().UTC().Add(-d)
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	// Build scope description for header.
	scopeParts := []string{}
	if *repo != "" {
		scopeParts = append(scopeParts, "repo="+*repo)
	}
	if *user != "" {
		scopeParts = append(scopeParts, "user="+*user)
	}
	if *since != "" {
		scopeParts = append(scopeParts, "since="+*since)
	}
	scope := "all sessions"
	if len(scopeParts) > 0 {
		scope = strings.Join(scopeParts, ", ")
	}

	fmt.Println("agentrun stats")
	fmt.Println("==============")
	fmt.Printf("Scope: %s\n", scope)

	// --- By agent ---
	agentStats, err := db.StatsByAgent(d, *repo, *user, sinceTime)
	if err != nil {
		return fmt.Errorf("stats: agent rollup: %w", err)
	}

	fmt.Println()
	fmt.Println("By agent:")
	if len(agentStats) == 0 {
		fmt.Println("  (no sessions)")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, a := range agentStats {
			fmt.Fprintf(tw, "  %s\t%d sessions\t%d tool calls\t%d validations failed\t%s DB\n",
				a.Agent, a.SessionCount, a.ToolCalls, a.ValidationsFail, humanBytes(a.BytesEstimate),
			)
		}
		tw.Flush()
	}

	// --- By repo ---
	repoStats, err := db.StatsByRepo(d, "", *user, sinceTime, 20)
	if err != nil {
		return fmt.Errorf("stats: repo rollup: %w", err)
	}

	fmt.Println()
	fmt.Println("By repo:")
	if len(repoStats) == 0 {
		fmt.Println("  (no sessions)")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, r := range repoStats {
			fmt.Fprintf(tw, "  %s\t%d sessions\t%d tool calls\n",
				r.RepoRoot, r.SessionCount, r.ToolCalls,
			)
		}
		tw.Flush()
	}

	// --- Top users ---
	userStats, err := db.StatsByUser(d, 10)
	if err != nil {
		return fmt.Errorf("stats: user rollup: %w", err)
	}

	fmt.Println()
	fmt.Println("Top users:")
	if len(userStats) == 0 {
		fmt.Println("  (no sessions)")
	} else {
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		for _, u := range userStats {
			fmt.Fprintf(tw, "  %s\t%d sessions\n", u.UserName, u.SessionCount)
		}
		tw.Flush()
	}

	// --- Recent failures ---
	failures, err := db.RecentValidationFailures(d, sinceTime, 10)
	if err != nil {
		return fmt.Errorf("stats: validation failures: %w", err)
	}

	fmt.Println()
	fmt.Printf("Recent failures (validations_fail > 0): %d\n", len(failures))
	for _, f := range failures {
		fmt.Printf("  %s  %s  failed at %s\n",
			truncate(f.SessionID, 20),
			truncate(f.Command, 30),
			f.FailedAt.Local().Format("2006-01-02 15:04"),
		)
	}

	return nil
}

// parseDurationWithDays parses a duration string that optionally uses 'd' for days.
// Examples: "7d", "24h", "30m", "1h30m", "2d12h"
func parseDurationWithDays(s string) (time.Duration, error) {
	// Replace 'd' suffix / occurrences with their equivalent in hours.
	// Strategy: scan for digit-runs followed by 'd', multiply by 24h.
	result := time.Duration(0)
	rest := s
	for rest != "" {
		// Try standard time.ParseDuration first.
		if d, err := time.ParseDuration(rest); err == nil {
			return result + d, nil
		}
		// Find a leading number followed by 'd'.
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		if i == 0 || i >= len(rest) || rest[i] != 'd' {
			// Fall back to standard parser for error message.
			return time.ParseDuration(s)
		}
		days := 0
		for _, ch := range rest[:i] {
			days = days*10 + int(ch-'0')
		}
		result += time.Duration(days) * 24 * time.Hour
		rest = rest[i+1:]
	}
	return result, nil
}
