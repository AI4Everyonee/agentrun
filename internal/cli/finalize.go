package cli

import (
	"flag"
	"fmt"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// runFinalizeIdle sweeps sessions where status='running' and whose last event
// is older than --older-than (default 30m), marks them completed.
// Usage: agentrun finalize-idle [--older-than <duration>]
func runFinalizeIdle(args []string) error {
	fs := flag.NewFlagSet("finalize-idle", flag.ContinueOnError)
	olderThan := fs.Duration("older-than", 30*time.Minute, "finalize sessions with last event older than this duration")

	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	if len(fs.Args()) > 0 {
		return ErrUsage
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

	count, err := db.FinalizeIdleSessions(d, *olderThan)
	if err != nil {
		return err
	}

	fmt.Printf("agentrun: finalized %d idle sessions older than %s\n", count, *olderThan)
	return nil
}
