package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/transcript"
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

	ids, err := db.FinalizeIdleSessions(d, *olderThan)
	if err != nil {
		return err
	}

	fmt.Printf("agentrun: finalized %d idle sessions older than %s\n", len(ids), *olderThan)

	// For each freshly finalized session: copy its transcript artifact (if
	// the path is still on disk) and schedule an OpenAI summary. Both are
	// best-effort; errors logged-and-swallowed.
	artifactsDir := filepath.Join(filepath.Dir(dbPath), "artifacts")
	for _, sid := range ids {
		sess, err := db.GetSession(d, sid)
		if err == nil && sess.TranscriptPath.Valid {
			if err := transcript.Capture(d, sid, sess.TranscriptPath.String, artifactsDir); err != nil {
				fmt.Fprintf(os.Stderr, "agentrun: capture transcript %s: %v\n", sid, err)
			}
		}
		scheduleSummary(sid)
	}
	return nil
}
