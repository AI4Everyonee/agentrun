package cli

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/summarizer"
)

// runSummarize implements `agentrun summarize <session_id> [--force]` and
// `agentrun summarize --recent N [--force]`. Called both interactively by
// users and detached from session-end paths (see scheduleSummary).
//
// Failures log to stderr and return nil — summarization is best-effort and
// must never propagate as an exit error to a parent that has already moved on.
func runSummarize(args []string) error {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	force := fs.Bool("force", false, "regenerate even if summary already exists")
	recent := fs.Int("recent", 0, "summarize the N most recent sessions (instead of one)")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	rest := fs.Args()

	dbPath, err := resolveDBPath()
	if err != nil {
		return fmt.Errorf("summarize: resolve db: %w", err)
	}
	d, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("summarize: open db: %w", err)
	}
	defer d.Close()

	switch {
	case *recent > 0:
		return summarizeRecent(d, *recent, *force)
	case len(rest) == 1:
		return summarizeOne(d, rest[0], *force)
	default:
		fmt.Fprintln(os.Stderr, "usage: agentrun summarize <session_id> [--force]")
		fmt.Fprintln(os.Stderr, "       agentrun summarize --recent N [--force]")
		return ErrUsage
	}
}

func summarizeOne(d *sql.DB, sessionID string, force bool) error {
	if !force {
		sess, err := db.GetSession(d, sessionID)
		if err == nil && sess.Summary.Valid && strings.TrimSpace(sess.Summary.String) != "" {
			fmt.Printf("agentrun: session %s already summarized; pass --force to redo\n", sessionID)
			return nil
		}
	}

	in, err := summarizer.BuildInput(d, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun summarize: build input: %v\n", err)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := summarizer.Summarize(ctx, in)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentrun summarize: %v\n", err)
		return nil
	}
	if res.Summary == "" {
		fmt.Fprintln(os.Stderr, "agentrun summarize: no summary produced (OPENAI_API_KEY unset or disabled)")
		return nil
	}
	if err := db.UpdateSessionSummary(d, sessionID, res.Summary, res.Model, int64(res.Tokens)); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun summarize: persist: %v\n", err)
		return nil
	}
	fmt.Printf("agentrun: summarized %s (%s, %d tokens)\n", sessionID, res.Model, res.Tokens)
	return nil
}

func summarizeRecent(d *sql.DB, n int, force bool) error {
	rows, err := db.ListSessions(d, n)
	if err != nil {
		return fmt.Errorf("summarize: list: %w", err)
	}
	for _, r := range rows {
		if err := summarizeOne(d, r.ID, force); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun summarize %s: %v\n", r.ID, err)
		}
	}
	return nil
}

// scheduleSummary fires `agentrun summarize <id>` detached (Setsid). Used from
// recorder.Close (wrapper sessions) and the hook subcommand on SessionEnd
// (native Claude sessions) so neither blocks for the OpenAI round-trip.
//
// Best-effort: failures here are swallowed because the parent has already
// finalized the session and is about to exit.
func scheduleSummary(sessionID string) {
	self, err := os.Executable()
	if err != nil || self == "" {
		return
	}
	if v := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); v == "" {
		// No API key — no point spawning a child that will immediately no-op.
		return
	}
	_ = spawnDetached(self, "summarize", sessionID)
}
