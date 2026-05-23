package cli

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/jeevan/agentrun/internal/config"
	"github.com/jeevan/agentrun/internal/db"
)

// runSessions lists the 50 most-recent sessions from the DB.
func runSessions(args []string) error {
	if len(args) > 0 {
		return ErrUsage
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

	rows, err := db.ListSessions(d, 50)
	if err != nil {
		return err
	}

	if len(rows) == 0 {
		fmt.Println("No sessions recorded yet.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SESSION ID\tAGENT\tREPO\tSTARTED\tSTATUS")
	for _, r := range rows {
		repo := r.RepoRoot.String
		if !r.RepoRoot.Valid || repo == "" {
			repo = r.Cwd
		}
		status := r.Status.String
		if !r.Status.Valid {
			status = "?"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			truncate(r.ID, 30),
			truncate(r.Agent, 8),
			truncate(repo, 28),
			r.StartedAt.Local().Format("2006-01-02 15:04:05"),
			truncate(status, 12),
		)
	}
	return w.Flush()
}

// truncate shortens s to at most n runes, appending "..." when truncated.
// If n <= 3, it returns the first n bytes with no ellipsis.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}
