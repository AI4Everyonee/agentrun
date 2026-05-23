// agentrun ingests Claude Code and Codex session JSONL transcripts into Postgres.
//
// The agents already write a complete transcript of every session to disk:
//
//	Claude → ~/.claude/projects/<derived>/<session-uuid>.jsonl
//	Codex  → ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
//
// This binary watches those files (or runs a one-shot catch-up) and stores
// each line as one row in `events`, with one row in `sessions` per file.
// That's the whole thing — no hooks, no PTY, no per-laptop install.
//
// Commands:
//
//	agentrun watch                  Tail JSONL files forever
//	agentrun sync [--since 14d]     One-shot backfill; exits when caught up
//	agentrun list                   Print recent sessions
//	agentrun show <session_uuid>    Print every event in one session
//
// Env:
//
//	DATABASE_URL   Postgres DSN (required)
//	AGENTRUN_USER  Identity stamped on every session (defaults to $USER)
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ─── Schema ──────────────────────────────────────────────────────────────────

const schemaSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS sessions (
    agent           TEXT        NOT NULL,
    session_uuid    TEXT        NOT NULL,
    user_name       TEXT,
    cwd             TEXT,
    model           TEXT,
    started_at      TIMESTAMPTZ NOT NULL,
    ended_at        TIMESTAMPTZ,
    transcript_path TEXT        NOT NULL,
    PRIMARY KEY (agent, session_uuid)
);

CREATE INDEX IF NOT EXISTS sessions_user_started
    ON sessions(user_name, started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_started
    ON sessions(started_at DESC);

CREATE TABLE IF NOT EXISTS events (
    id              BIGSERIAL   PRIMARY KEY,
    agent           TEXT        NOT NULL,
    session_uuid    TEXT        NOT NULL,
    seq             INT         NOT NULL,
    ts              TIMESTAMPTZ NOT NULL,
    role            TEXT        NOT NULL,
    tool_name       TEXT,
    content         TEXT,
    payload         JSONB       NOT NULL,
    FOREIGN KEY (agent, session_uuid)
        REFERENCES sessions(agent, session_uuid)
        ON DELETE CASCADE,
    UNIQUE (agent, session_uuid, seq)
);

CREATE INDEX IF NOT EXISTS events_session   ON events(agent, session_uuid, seq);
CREATE INDEX IF NOT EXISTS events_role      ON events(role);
CREATE INDEX IF NOT EXISTS events_ts        ON events(ts);
CREATE INDEX IF NOT EXISTS events_tool      ON events(tool_name) WHERE tool_name IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_payload_gin
    ON events USING gin(payload jsonb_path_ops);

CREATE TABLE IF NOT EXISTS ingest_state (
    file_path   TEXT        PRIMARY KEY,
    inode       BIGINT      NOT NULL,
    byte_offset BIGINT      NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// ─── Domain types ────────────────────────────────────────────────────────────

type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

type Role string

const (
	RoleUser       Role = "user"
	RoleAssistant  Role = "assistant"
	RoleToolUse    Role = "tool_use"
	RoleToolResult Role = "tool_result"
	RoleSystem     Role = "system"
)

type Session struct {
	Agent          Agent
	UUID           string
	UserName       string
	Cwd            string
	Model          string
	StartedAt      time.Time
	EndedAt        time.Time
	TranscriptPath string
}

type Event struct {
	Seq      int
	Ts       time.Time
	Role     Role
	ToolName string
	Content  string
	Payload  []byte
}

type roots struct {
	Claude string
	Codex  string
}

// ─── main / dispatch ─────────────────────────────────────────────────────────

var errUsage = errors.New("usage")

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: %v\n", err)
		if errors.Is(err, errUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return printUsage()
	}
	switch args[0] {
	case "watch":
		return cmdWatch(args[1:])
	case "sync":
		return cmdSync(args[1:])
	case "list":
		return cmdList(args[1:])
	case "show":
		return cmdShow(args[1:])
	case "help", "-h", "--help":
		return printUsage()
	default:
		return fmt.Errorf("unknown command: %q (try 'agentrun help')", args[0])
	}
}

func printUsage() error {
	fmt.Println(`agentrun — ingest Claude Code and Codex sessions into Postgres

Usage:
  agentrun watch                 Tail JSONL files and ingest forever
  agentrun sync [--since 14d]    One-shot catch-up; exits when caught up
  agentrun list                  Show recent sessions
  agentrun show <session_uuid>   Show events in one session

Env:
  DATABASE_URL    Postgres DSN (required)
                  example: postgres://agentrun:agentrun@localhost:5433/agentrun
  AGENTRUN_USER   Identity stamped on every session (defaults to $USER)`)
	return errUsage
}

// ─── Subcommands ─────────────────────────────────────────────────────────────

func cmdWatch(args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	rt, err := defaultRoots()
	if err != nil {
		return err
	}
	user := detectUser()
	fmt.Printf("agentrun: user=%q\n", user)
	fmt.Printf("agentrun: watching %s\n", rt.Claude)
	fmt.Printf("agentrun: watching %s\n", rt.Codex)
	fmt.Println("agentrun: catching up first…")
	return runWatcher(ctx, pool, rt, user)
}

func cmdSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	sinceStr := fs.String("since", "14d", "only ingest files modified more recently (e.g. 14d, 24h, 30m); empty = no limit")
	if err := fs.Parse(args); err != nil {
		return errUsage
	}
	if len(fs.Args()) > 0 {
		return errUsage
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

	ctx := context.Background()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	rt, err := defaultRoots()
	if err != nil {
		return err
	}
	return initialSweep(ctx, pool, rt, detectUser(), since)
}

func cmdList(args []string) error {
	if len(args) > 0 {
		return errUsage
	}
	ctx := context.Background()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	rows, err := pool.Query(ctx, `
SELECT s.agent, s.session_uuid, COALESCE(s.user_name, '-'), COALESCE(s.model, '-'),
       s.started_at, COALESCE(s.ended_at, s.started_at),
       (SELECT COUNT(*) FROM events e WHERE e.agent = s.agent AND e.session_uuid = s.session_uuid)
FROM sessions s ORDER BY s.started_at DESC LIMIT 30`)
	if err != nil {
		return err
	}
	defer rows.Close()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSESSION\tUSER\tMODEL\tSTARTED\tDURATION\tEVENTS")
	for rows.Next() {
		var agent, uuid, user, model string
		var startedAt, endedAt time.Time
		var n int
		if err := rows.Scan(&agent, &uuid, &user, &model, &startedAt, &endedAt, &n); err != nil {
			return err
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\n",
			agent, shortID(uuid), user, model,
			startedAt.Local().Format("2006-01-02 15:04"),
			endedAt.Sub(startedAt).Round(time.Second), n)
	}
	return w.Flush()
}

func cmdShow(args []string) error {
	if len(args) != 1 {
		return errUsage
	}
	sid := args[0]
	ctx := context.Background()
	pool, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()

	var agent, user, model, cwd, transcript string
	var startedAt, endedAt time.Time
	row := pool.QueryRow(ctx, `
SELECT agent, COALESCE(user_name,'-'), COALESCE(model,'-'), COALESCE(cwd,'-'),
       transcript_path, started_at, COALESCE(ended_at, started_at)
FROM sessions WHERE session_uuid = $1`, sid)
	if err := row.Scan(&agent, &user, &model, &cwd, &transcript, &startedAt, &endedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("session %q not found", sid)
		}
		return err
	}

	fmt.Printf("Session: %s (%s)\nUser:    %s\nModel:   %s\nCwd:     %s\nStarted: %s\nEnded:   %s\nSource:  %s\n\n",
		sid, agent, user, model, cwd,
		startedAt.Local().Format("2006-01-02 15:04:05"),
		endedAt.Local().Format("2006-01-02 15:04:05"),
		transcript)

	rows, err := pool.Query(ctx, `
SELECT seq, ts, role, COALESCE(tool_name,''), COALESCE(content,'')
FROM events WHERE agent = $1 AND session_uuid = $2 ORDER BY seq`, agent, sid)
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
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", seq, ts.Local().Format("15:04:05.000"), role, detail)
	}
	return w.Flush()
}

// ─── DB ──────────────────────────────────────────────────────────────────────

func openDB(ctx context.Context) (*pgxpool.Pool, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return nil, fmt.Errorf("DATABASE_URL is not set")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return pool, nil
}

func upsertSession(ctx context.Context, pool *pgxpool.Pool, s Session) error {
	const q = `
INSERT INTO sessions (agent, session_uuid, user_name, cwd, model, started_at, ended_at, transcript_path)
VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), NULLIF($5,''), $6, $7, $8)
ON CONFLICT (agent, session_uuid) DO UPDATE SET
    user_name       = COALESCE(sessions.user_name, EXCLUDED.user_name),
    cwd             = COALESCE(sessions.cwd, EXCLUDED.cwd),
    model           = COALESCE(sessions.model, EXCLUDED.model),
    started_at      = LEAST(sessions.started_at, EXCLUDED.started_at),
    ended_at        = GREATEST(sessions.ended_at, EXCLUDED.ended_at),
    transcript_path = EXCLUDED.transcript_path`
	var endedAt any
	if !s.EndedAt.IsZero() {
		endedAt = s.EndedAt
	}
	_, err := pool.Exec(ctx, q,
		string(s.Agent), s.UUID, s.UserName, s.Cwd, s.Model,
		s.StartedAt, endedAt, s.TranscriptPath)
	return err
}

func insertEventsAndCheckpoint(ctx context.Context, pool *pgxpool.Pool,
	agent Agent, sid string, events []Event, file string, inode, off int64) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if len(events) > 0 {
		const q = `
INSERT INTO events (agent, session_uuid, seq, ts, role, tool_name, content, payload)
VALUES ($1,$2,$3,$4,$5, NULLIF($6,''), NULLIF($7,''), $8)
ON CONFLICT (agent, session_uuid, seq) DO NOTHING`
		for _, e := range events {
			payload := sanitizeJSON(e.Payload)
			if _, err := tx.Exec(ctx, q,
				string(agent), sid, e.Seq, e.Ts, string(e.Role),
				e.ToolName, sanitizeText(e.Content), payload); err != nil {
				return fmt.Errorf("insert seq=%d (%d B): %w", e.Seq, len(payload), err)
			}
		}
	}

	const upState = `
INSERT INTO ingest_state (file_path, inode, byte_offset) VALUES ($1,$2,$3)
ON CONFLICT (file_path) DO UPDATE SET inode = EXCLUDED.inode, byte_offset = EXCLUDED.byte_offset, updated_at = now()`
	if _, err := tx.Exec(ctx, upState, file, inode, off); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func loadIngestState(ctx context.Context, pool *pgxpool.Pool, file string) (inode, off int64, ok bool, err error) {
	err = pool.QueryRow(ctx, `SELECT inode, byte_offset FROM ingest_state WHERE file_path = $1`, file).
		Scan(&inode, &off)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return inode, off, true, nil
}

// nulJSONEscape is the 6-byte sequence backslash-u-0-0-0-0, built numerically
// so neither Go's string-escape rules nor any code-generation tool can mangle
// it. Postgres's JSONB parser rejects this escape (even properly double-escaped
// inside JSON strings) because PG TEXT can't store NUL. We replace each
// occurrence with the same-length escape for U+0020 (space) so the surrounding
// JSON stays valid (a literal-space substitution would break `\\u0000` into
// `\ ` which is not a valid JSON escape).
var nulJSONEscape = []byte{0x5c, 0x75, 0x30, 0x30, 0x30, 0x30}
var spaceJSONEscape = []byte{0x5c, 0x75, 0x30, 0x30, 0x32, 0x30}

func sanitizeJSON(b []byte) []byte {
	if !bytes.ContainsAny(b, "\x00") && !bytes.Contains(b, nulJSONEscape) {
		return b
	}
	out := bytes.ReplaceAll(b, []byte{0x00}, []byte{' '})
	out = bytes.ReplaceAll(out, nulJSONEscape, spaceJSONEscape)
	return out
}

func sanitizeText(s string) string {
	if !strings.ContainsRune(s, 0) {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// ─── Watcher ─────────────────────────────────────────────────────────────────

func defaultRoots() (roots, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return roots{}, err
	}
	return roots{
		Claude: filepath.Join(home, ".claude", "projects"),
		Codex:  filepath.Join(home, ".codex", "sessions"),
	}, nil
}

func runWatcher(ctx context.Context, pool *pgxpool.Pool, rt roots, user string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	addRecursive(w, rt.Claude)
	addRecursive(w, rt.Codex)

	if err := initialSweep(ctx, pool, rt, user, time.Time{}); err != nil {
		return err
	}

	const debounce = 200 * time.Millisecond
	pending := map[string]time.Time{}
	var mu sync.Mutex
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			handleFsEvent(w, ev, &mu, pending)
		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintf(os.Stderr, "agentrun: watch error: %v\n", err)
		case <-tick.C:
			cutoff := time.Now().Add(-debounce)
			for _, p := range drainPending(&mu, pending, cutoff) {
				if err := ingestFile(ctx, pool, rt, p, user); err != nil {
					fmt.Fprintf(os.Stderr, "agentrun: ingest %s: %v\n", p, err)
				}
			}
		}
	}
}

func handleFsEvent(w *fsnotify.Watcher, ev fsnotify.Event, mu *sync.Mutex, pending map[string]time.Time) {
	if ev.Op&fsnotify.Create != 0 {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.Add(ev.Name)
			return
		}
	}
	if !strings.HasSuffix(ev.Name, ".jsonl") || ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
		return
	}
	mu.Lock()
	if _, ok := pending[ev.Name]; !ok {
		pending[ev.Name] = time.Now()
	}
	mu.Unlock()
}

func drainPending(mu *sync.Mutex, pending map[string]time.Time, cutoff time.Time) []string {
	mu.Lock()
	defer mu.Unlock()
	var out []string
	for p, ts := range pending {
		if ts.Before(cutoff) {
			out = append(out, p)
			delete(pending, p)
		}
	}
	return out
}

func addRecursive(w *fsnotify.Watcher, root string) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = w.Add(path)
		}
		return nil
	})
}

func initialSweep(ctx context.Context, pool *pgxpool.Pool, rt roots, user string, since time.Time) error {
	for _, root := range []string{rt.Claude, rt.Codex} {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if !since.IsZero() {
				fi, err := d.Info()
				if err == nil && fi.ModTime().Before(since) {
					return nil
				}
			}
			if err := ingestFile(ctx, pool, rt, path, user); err != nil {
				fmt.Fprintf(os.Stderr, "agentrun: sweep %s: %v\n", path, err)
			}
			return nil
		})
	}
	return nil
}

func ingestFile(ctx context.Context, pool *pgxpool.Pool, rt roots, path, user string) error {
	agent := detectAgent(rt, path)
	if agent == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	inode := fileInode(fi)

	oldInode, offset, ok, err := loadIngestState(ctx, pool, path)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	if ok && oldInode != inode {
		offset = 0 // file was rotated
	}
	if offset > fi.Size() {
		offset = 0 // file was truncated
	}
	if offset == fi.Size() {
		return nil // nothing new
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}

	prevLines, err := countLines(path, offset)
	if err != nil {
		return err
	}

	sess := Session{Agent: agent, TranscriptPath: path, UserName: user}
	parser := pickParser(agent)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 64<<20)
	var events []Event
	bytesRead := offset
	seq := prevLines + 1
	for scanner.Scan() {
		line := scanner.Bytes()
		bytesRead += int64(len(line)) + 1
		ev, ok := parser(line, seq, &sess)
		seq++
		if !ok {
			continue
		}
		events = append(events, ev)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	if bytesRead > fi.Size() {
		bytesRead = fi.Size()
	}

	if sess.UUID == "" {
		fmt.Fprintf(os.Stderr, "agentrun: %s has no session UUID — skipping\n", path)
		return nil
	}

	if err := upsertSession(ctx, pool, sess); err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	return insertEventsAndCheckpoint(ctx, pool, agent, sess.UUID, events, path, inode, bytesRead)
}

func countLines(path string, upTo int64) (int, error) {
	if upTo == 0 {
		return 0, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := io.LimitReader(f, upTo)
	buf := make([]byte, 64<<10)
	n := 0
	for {
		read, err := r.Read(buf)
		for i := 0; i < read; i++ {
			if buf[i] == '\n' {
				n++
			}
		}
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func detectAgent(rt roots, path string) Agent {
	switch {
	case rt.Claude != "" && strings.HasPrefix(path, rt.Claude):
		return AgentClaude
	case rt.Codex != "" && strings.HasPrefix(path, rt.Codex):
		return AgentCodex
	}
	return ""
}

func pickParser(a Agent) func([]byte, int, *Session) (Event, bool) {
	if a == AgentCodex {
		return parseCodexLine
	}
	return parseClaudeLine
}

func fileInode(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Ino)
	}
	return 0
}

// ─── Claude parser ───────────────────────────────────────────────────────────

func parseClaudeLine(line []byte, seq int, sess *Session) (Event, bool) {
	if len(line) == 0 {
		return Event{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}

	ev := Event{Seq: seq, Payload: append([]byte(nil), line...)}
	if ts, ok := raw["timestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.Ts = t
		}
	}
	if cwd, ok := raw["cwd"].(string); ok && sess.Cwd == "" {
		sess.Cwd = cwd
	}
	if id, ok := raw["sessionId"].(string); ok && sess.UUID == "" {
		sess.UUID = id
	}

	switch typ, _ := raw["type"].(string); typ {
	case "user":
		ev.Role = RoleUser
		ev.Content = claudeMessageText(raw)
	case "assistant":
		if name, input, ok := claudeToolUse(raw); ok {
			ev.Role = RoleToolUse
			ev.ToolName = name
			ev.Content = input
		} else {
			ev.Role = RoleAssistant
			ev.Content = claudeMessageText(raw)
			if m := claudeModel(raw); m != "" && sess.Model == "" {
				sess.Model = m
			}
		}
	default:
		ev.Role = RoleSystem
	}
	if sess.StartedAt.IsZero() && !ev.Ts.IsZero() {
		sess.StartedAt = ev.Ts
	}
	if !ev.Ts.IsZero() && ev.Ts.After(sess.EndedAt) {
		sess.EndedAt = ev.Ts
	}
	return ev, true
}

func claudeMessageText(raw map[string]any) string {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var out string
		for _, block := range c {
			b, _ := block.(map[string]any)
			if b == nil {
				continue
			}
			if t, _ := b["type"].(string); t != "text" {
				continue
			}
			if text, _ := b["text"].(string); text != "" {
				if out != "" {
					out += "\n"
				}
				out += text
			}
		}
		return out
	}
	return ""
}

func claudeToolUse(raw map[string]any) (name, inputJSON string, ok bool) {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return "", "", false
	}
	blocks, _ := msg["content"].([]any)
	for _, block := range blocks {
		b, _ := block.(map[string]any)
		if b == nil {
			continue
		}
		if t, _ := b["type"].(string); t != "tool_use" {
			continue
		}
		name, _ = b["name"].(string)
		if input, ok := b["input"]; ok {
			if buf, err := json.Marshal(input); err == nil {
				inputJSON = string(buf)
			}
		}
		return name, inputJSON, true
	}
	return "", "", false
}

func claudeModel(raw map[string]any) string {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	m, _ := msg["model"].(string)
	return m
}

// ─── Codex parser ────────────────────────────────────────────────────────────

func parseCodexLine(line []byte, seq int, sess *Session) (Event, bool) {
	if len(line) == 0 {
		return Event{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}

	ev := Event{Seq: seq, Payload: append([]byte(nil), line...)}
	if ts, ok := raw["timestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.Ts = t
		}
	}

	outer, _ := raw["type"].(string)
	payload, _ := raw["payload"].(map[string]any)

	if outer == "session_meta" && payload != nil {
		if id, ok := payload["id"].(string); ok && sess.UUID == "" {
			sess.UUID = id
		}
		if cwd, ok := payload["cwd"].(string); ok && sess.Cwd == "" {
			sess.Cwd = cwd
		}
		if m, ok := payload["model"].(string); ok && sess.Model == "" {
			sess.Model = m
		}
	}
	if outer == "turn_context" && payload != nil && sess.Model == "" {
		if m, ok := payload["model"].(string); ok {
			sess.Model = m
		}
	}

	inner := ""
	if payload != nil {
		inner, _ = payload["type"].(string)
	}

	switch {
	case outer == "event_msg" && inner == "user_message":
		ev.Role = RoleUser
		if payload != nil {
			ev.Content, _ = payload["message"].(string)
			if ev.Content == "" {
				ev.Content, _ = payload["text"].(string)
			}
		}
	case outer == "event_msg" && inner == "agent_message":
		ev.Role = RoleAssistant
		if payload != nil {
			ev.Content, _ = payload["message"].(string)
			if ev.Content == "" {
				ev.Content, _ = payload["text"].(string)
			}
		}
	case outer == "response_item" && inner == "function_call":
		ev.Role = RoleToolUse
		if payload != nil {
			ev.ToolName, _ = payload["name"].(string)
			if args, ok := payload["arguments"].(string); ok {
				ev.Content = args
			} else if argsObj, ok := payload["arguments"].(map[string]any); ok {
				if b, err := json.Marshal(argsObj); err == nil {
					ev.Content = string(b)
				}
			}
		}
	case outer == "response_item" && inner == "function_call_output":
		ev.Role = RoleToolResult
		if payload != nil {
			ev.ToolName, _ = payload["name"].(string)
			ev.Content, _ = payload["output"].(string)
		}
	case outer == "response_item" && inner == "message":
		role := ""
		if payload != nil {
			role, _ = payload["role"].(string)
		}
		switch role {
		case "user":
			ev.Role = RoleUser
		case "assistant":
			ev.Role = RoleAssistant
		default:
			ev.Role = RoleSystem
		}
		ev.Content = codexMessageText(payload)
	default:
		ev.Role = RoleSystem
	}

	if sess.StartedAt.IsZero() && !ev.Ts.IsZero() {
		sess.StartedAt = ev.Ts
	}
	if !ev.Ts.IsZero() && ev.Ts.After(sess.EndedAt) {
		sess.EndedAt = ev.Ts
	}
	return ev, true
}

func codexMessageText(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	blocks, _ := payload["content"].([]any)
	var out string
	for _, block := range blocks {
		b, _ := block.(map[string]any)
		if b == nil {
			continue
		}
		typ, _ := b["type"].(string)
		if typ != "input_text" && typ != "output_text" && typ != "text" {
			continue
		}
		if text, _ := b["text"].(string); text != "" {
			if out != "" {
				out += "\n"
			}
			out += text
		}
	}
	return out
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func detectUser() string {
	for _, k := range []string{"AGENTRUN_USER", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func parseDuration(s string) (time.Duration, error) {
	if i := strings.IndexByte(s, 'd'); i > 0 {
		days, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("invalid day count %q", s)
		}
		rest := time.Duration(0)
		if i+1 < len(s) {
			r, err := time.ParseDuration(s[i+1:])
			if err != nil {
				return 0, err
			}
			rest = r
		}
		return time.Duration(days)*24*time.Hour + rest, nil
	}
	return time.ParseDuration(s)
}

func shortID(uuid string) string {
	if len(uuid) <= 12 {
		return uuid
	}
	return uuid[:12] + "…"
}
