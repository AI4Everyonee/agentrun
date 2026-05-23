// Package cli implements the subcommand dispatch and the human-facing
// commands (list, show). Each command is one function, all in this file.
//
// Four commands total:
//
//	agentrun watch          — long-running daemon
//	agentrun sync           — one-shot catch-up; exits when caught up
//	agentrun list           — print recent sessions
//	agentrun show <sid>     — print events for one session
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/watch"
)

// ErrUsage signals a malformed CLI invocation. main() maps it to exit 2.
var ErrUsage = errors.New("usage")

// Run dispatches based on args[0]. args excludes argv[0].
func Run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "watch":
		return runWatch(args[1:])
	case "sync":
		return runSync(args[1:])
	case "list":
		return runList(args[1:])
	case "show":
		return runShow(args[1:])
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command: %q (try 'agentrun help')", args[0])
	}
}

func usage() error {
	fmt.Println(`agentrun — ingest Claude Code and Codex sessions into Postgres

Usage:
  agentrun watch                 Tail JSONL files and ingest forever
  agentrun sync                  One-shot catch-up; exits when caught up
  agentrun list                  Show recent sessions
  agentrun show <session_uuid>   Show events in one session

Env:
  DATABASE_URL    Postgres DSN (required)
                  example: postgres://agentrun:agentrun@localhost:5433/agentrun
  AGENTRUN_USER   Identity stamped on every session (defaults to $USER)`)
	return ErrUsage
}

func openDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set (example: postgres://agentrun:agentrun@localhost:5433/agentrun)")
	}
	return db.Open(ctx, dsn)
}

func detectUserName() string {
	for _, k := range []string{"AGENTRUN_USER", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func runWatch(args []string) error {
	if len(args) > 0 {
		return ErrUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	roots, err := watch.DefaultRoots()
	if err != nil {
		return err
	}
	user := detectUserName()
	fmt.Printf("agentrun: user=%q\n", user)
	fmt.Printf("agentrun: watching %s\n", roots.ClaudeProjects)
	fmt.Printf("agentrun: watching %s\n", roots.CodexSessions)
	fmt.Println("agentrun: catching up first…")
	return watch.Run(ctx, pool, roots, user)
}

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	sinceStr := fs.String("since", "14d", "only ingest files modified more recently than this (e.g. 14d, 24h, 30m). Empty string = no limit.")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}
	if len(fs.Args()) > 0 {
		return ErrUsage
	}

	var since time.Time
	if *sinceStr != "" {
		dur, err := parseDuration(*sinceStr)
		if err != nil {
			return fmt.Errorf("--since: %w", err)
		}
		since = time.Now().Add(-dur)
		fmt.Printf("agentrun: ingesting files modified since %s\n", since.Local().Format("2006-01-02 15:04"))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	roots, err := watch.DefaultRoots()
	if err != nil {
		return err
	}
	return watch.Sync(ctx, pool, roots, detectUserName(), since)
}

// parseDuration extends time.ParseDuration to accept a trailing 'd' for days.
// "14d" → 14×24h. Compound forms like "1d12h" also work.
func parseDuration(s string) (time.Duration, error) {
	if i := strings.IndexByte(s, 'd'); i > 0 {
		days, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("invalid day count in %q", s)
		}
		rest := time.Duration(0)
		if i+1 < len(s) {
			r, err := time.ParseDuration(s[i+1:])
			if err != nil {
				return 0, fmt.Errorf("invalid remainder %q: %w", s[i+1:], err)
			}
			rest = r
		}
		return time.Duration(days)*24*time.Hour + rest, nil
	}
	return time.ParseDuration(s)
}

func runList(args []string) error {
	if len(args) > 0 {
		return ErrUsage
	}
	ctx := context.Background()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
SELECT s.agent, s.session_uuid,
       COALESCE(s.user_name, '-'),
       COALESCE(s.model, '-'),
       s.started_at,
       COALESCE(s.ended_at, s.started_at),
       (SELECT COUNT(*) FROM events e
          WHERE e.agent = s.agent AND e.session_uuid = s.session_uuid)
FROM sessions s
ORDER BY s.started_at DESC
LIMIT 30`)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSESSION\tUSER\tMODEL\tSTARTED\tDURATION\tEVENTS")
	for rows.Next() {
		var agent, uuid, user, model string
		var startedAt, endedAt time.Time
		var nEvents int
		if err := rows.Scan(&agent, &uuid, &user, &model, &startedAt, &endedAt, &nEvents); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			agent, shortID(uuid), user, model,
			startedAt.Local().Format("2006-01-02 15:04"),
			endedAt.Sub(startedAt).Round(time.Second),
			nEvents,
		)
	}
	return w.Flush()
}

func runShow(args []string) error {
	if len(args) != 1 {
		return ErrUsage
	}
	sessionUUID := args[0]
	ctx := context.Background()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	var agent, user, model, cwd, transcript string
	var startedAt, endedAt time.Time
	row := pool.QueryRow(ctx, `
SELECT agent,
       COALESCE(user_name, '-'),
       COALESCE(model, '-'),
       COALESCE(cwd, '-'),
       transcript_path,
       started_at,
       COALESCE(ended_at, started_at)
FROM sessions WHERE session_uuid = $1`, sessionUUID)
	if err := row.Scan(&agent, &user, &model, &cwd, &transcript, &startedAt, &endedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("session %q not found", sessionUUID)
		}
		return err
	}

	fmt.Printf("Session: %s (%s)\n", sessionUUID, agent)
	fmt.Printf("User:    %s\n", user)
	fmt.Printf("Model:   %s\n", model)
	fmt.Printf("Cwd:     %s\n", cwd)
	fmt.Printf("Started: %s\n", startedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Ended:   %s\n", endedAt.Local().Format("2006-01-02 15:04:05"))
	fmt.Printf("Source:  %s\n", transcript)
	fmt.Println()

	rows, err := pool.Query(ctx, `
SELECT seq, ts, role, COALESCE(tool_name, ''), COALESCE(content, '')
FROM events WHERE agent = $1 AND session_uuid = $2
ORDER BY seq`, agent, sessionUUID)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SEQ\tTIME\tROLE\tDETAIL")
	for rows.Next() {
		var seq int
		var ts time.Time
		var role, tool, content string
		if err := rows.Scan(&seq, &ts, &role, &tool, &content); err != nil {
			return err
		}
		detail := content
		if tool != "" {
			detail = tool + ": " + content
		}
		detail = strings.ReplaceAll(detail, "\n", " ⏎ ")
		if len(detail) > 100 {
			detail = detail[:97] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
			seq, ts.Local().Format("15:04:05.000"), role, detail)
	}
	return w.Flush()
}

func shortID(uuid string) string {
	if len(uuid) <= 12 {
		return uuid
	}
	return uuid[:12] + "…"
}
