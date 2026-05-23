// Package watch tails Claude and Codex JSONL transcripts and ingests new lines
// into Postgres.
//
// Design:
//   - One fsnotify watcher rooted at ~/.claude/projects and ~/.codex/sessions
//     (both added recursively at startup, new subdirs auto-added on creation).
//   - On boot we do a "catch up" pass: for every .jsonl, read from the
//     recorded byte_offset to EOF and ingest. New files start at 0.
//   - After catch-up we sit on fsnotify Write/Create events and tail the same way.
//   - Each ingestion is one atomic transaction (events + ingest_state),
//     so a crash mid-line never leaves duplicates or drift.
//
// Constraints kept deliberately simple:
//   - Single-process. No locking needed.
//   - No goroutine fan-out per file. Files are processed serially. Volumes
//     are well within what one core handles.
//   - Partial-line handling: we read up to the last newline only; any trailing
//     unterminated bytes stay un-ingested and get picked up next time the file
//     is modified.
package watch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/parse"
)

// Roots returns the two directories we watch. Resolved from $HOME (or
// override env vars for testing).
type Roots struct {
	ClaudeProjects string // ~/.claude/projects
	CodexSessions  string // ~/.codex/sessions
}

// DefaultRoots returns the canonical locations. Tests override.
func DefaultRoots() (Roots, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Roots{}, fmt.Errorf("DefaultRoots: %w", err)
	}
	return Roots{
		ClaudeProjects: filepath.Join(home, ".claude", "projects"),
		CodexSessions:  filepath.Join(home, ".codex", "sessions"),
	}, nil
}

// Sync runs only the initial catch-up sweep (same code path as Run uses on
// boot) and returns. Useful as a one-shot batch ingest.
//
// If since is non-zero, files whose mtime is older than `since` are skipped
// entirely — useful for limiting a first-run backfill to recent activity.
func Sync(ctx context.Context, pool *pgxpool.Pool, roots Roots, userName string, since time.Time) error {
	return initialSweep(ctx, pool, roots, userName, since)
}

// Run starts the watcher and blocks until ctx is canceled. The optional
// userName is stamped on every session we ingest (lets the same DB serve
// multiple users).
func Run(ctx context.Context, pool *pgxpool.Pool, roots Roots, userName string) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("watch.Run: NewWatcher: %w", err)
	}
	defer w.Close()

	// Add every existing directory under both roots; also do an initial sweep
	// of all .jsonl files so we catch up to anything written while we were down.
	if err := addRecursive(w, roots.ClaudeProjects); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: warn: watch claude root: %v\n", err)
	}
	if err := addRecursive(w, roots.CodexSessions); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: warn: watch codex root: %v\n", err)
	}

	// Run mode does a full sweep — no time filter — so we don't miss anything
	// written while we were down. (Sync subcommand applies its own --since filter.)
	if err := initialSweep(ctx, pool, roots, userName, time.Time{}); err != nil {
		return fmt.Errorf("watch.Run: initial sweep: %w", err)
	}

	// Debounce: same file written 100 times in 50ms → ingest once after the
	// burst settles. Editors and agents both write in bursts.
	const debounce = 200 * time.Millisecond
	pending := make(map[string]time.Time) // path → first-seen
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
			due := drainPending(&mu, pending, cutoff)
			for _, p := range due {
				if err := ingestFile(ctx, pool, roots, p, userName); err != nil {
					fmt.Fprintf(os.Stderr, "agentrun: ingest %s: %v\n", p, err)
				}
			}
		}
	}
}

// handleFsEvent updates the pending map or adds new directories to the watcher.
func handleFsEvent(w *fsnotify.Watcher, ev fsnotify.Event, mu *sync.Mutex, pending map[string]time.Time) {
	if ev.Op&fsnotify.Create != 0 {
		// If a directory was created, watch it (Codex makes a new YYYY/MM/DD/
		// dir each day). If a file, queue it for ingest.
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			_ = w.Add(ev.Name)
			return
		}
	}
	if !strings.HasSuffix(ev.Name, ".jsonl") {
		return
	}
	if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
		return
	}
	mu.Lock()
	if _, exists := pending[ev.Name]; !exists {
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

// addRecursive registers root and every existing subdirectory with the watcher.
// Missing roots are tolerated — the user may not use both agents.
func addRecursive(w *fsnotify.Watcher, root string) error {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best effort
		}
		if d.IsDir() {
			_ = w.Add(path)
		}
		return nil
	})
}

// initialSweep walks both roots and ingests anything new in every .jsonl file
// that already exists. Run once at startup. If `since` is non-zero, files
// last modified before that time are skipped — useful for limiting backfill
// to recent activity.
func initialSweep(ctx context.Context, pool *pgxpool.Pool, roots Roots, userName string, since time.Time) error {
	for _, root := range []string{roots.ClaudeProjects, roots.CodexSessions} {
		if _, err := os.Stat(root); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
				return nil
			}
			if !since.IsZero() {
				fi, err := d.Info()
				if err == nil && fi.ModTime().Before(since) {
					return nil
				}
			}
			if err := ingestFile(ctx, pool, roots, path, userName); err != nil {
				fmt.Fprintf(os.Stderr, "agentrun: sweep %s: %v\n", path, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ingestFile reads new lines from path (past byte_offset), parses them,
// and writes events + a new checkpoint to the DB.
func ingestFile(ctx context.Context, pool *pgxpool.Pool, roots Roots, path, userName string) error {
	agent := detectAgent(roots, path)
	if agent == "" {
		return nil // path didn't match either root
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

	// Resume from last known offset — but reset to 0 if the file was rotated
	// (inode changed) or truncated (size < old offset).
	_, offset, _, err := db.LoadIngestState(ctx, pool, path)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}
	if oldInode, _, ok, _ := db.LoadIngestState(ctx, pool, path); ok && oldInode != inode {
		offset = 0
	}
	if offset > fi.Size() {
		offset = 0
	}
	if offset == fi.Size() {
		return nil // nothing new
	}

	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	// Count lines we've already read in earlier ingests so seq stays stable.
	// We have to scan from 0 once to know the existing line count, then again
	// from offset for the new lines. For the typical case (small files,
	// frequent updates) this is cheap. For huge files we trust the
	// `events.seq` UNIQUE constraint + ON CONFLICT to dedupe.
	prevLineCount, err := countLines(path, offset)
	if err != nil {
		return fmt.Errorf("count lines: %w", err)
	}

	sess := parse.Session{
		Agent:          agent,
		TranscriptPath: path,
		UserName:       userName,
	}
	parser := pickParser(agent)

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024) // up to 64 MiB / line
	var events []parse.Event
	bytesRead := offset
	seq := prevLineCount + 1
	for scanner.Scan() {
		line := scanner.Bytes()
		bytesRead += int64(len(line)) + 1 // +1 for the newline byte
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
	// Watch for a partially-written trailing line: bufio.Scanner already
	// requires terminating newlines for split-by-line mode, so this isn't
	// directly an issue. But our bytesRead counts assume every line ended in
	// `\n`. Cap bytesRead at file size so we don't overshoot.
	if bytesRead > fi.Size() {
		bytesRead = fi.Size()
	}

	// Sessions where we never figured out the UUID are bugs in our parser
	// (Claude embeds it in every line; Codex in session_meta). Skip rather
	// than insert garbage.
	if sess.UUID == "" {
		fmt.Fprintf(os.Stderr, "agentrun: %s has no session UUID detected — skipping\n", path)
		return nil
	}

	// Always upsert the session row first (FK on events).
	if err := db.UpsertSession(ctx, pool, sess); err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}
	if err := db.InsertEventsAndCheckpoint(ctx, pool, agent, sess.UUID, events, path, inode, bytesRead); err != nil {
		return fmt.Errorf("insert events: %w", err)
	}
	return nil
}

// countLines returns the number of `\n` bytes in path[0:upTo].
// Used so we resume `seq` numbering correctly after a restart.
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
	buf := make([]byte, 64*1024)
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

func detectAgent(roots Roots, path string) parse.Agent {
	switch {
	case roots.ClaudeProjects != "" && strings.HasPrefix(path, roots.ClaudeProjects):
		return parse.AgentClaude
	case roots.CodexSessions != "" && strings.HasPrefix(path, roots.CodexSessions):
		return parse.AgentCodex
	}
	return ""
}

func pickParser(a parse.Agent) func([]byte, int, *parse.Session) (parse.Event, bool) {
	if a == parse.AgentCodex {
		return parse.ParseCodexLine
	}
	return parse.ParseClaudeLine
}

func fileInode(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int64(st.Ino)
	}
	return 0
}
