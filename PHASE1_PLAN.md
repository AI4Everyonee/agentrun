# agentrun — Phase 1 Implementation Plan

## 0. Scope statement (read once)

This document is the **only** input required to build Phase 1 of `agentrun`. It supersedes any free-form reading of `GOAL.md`. Phase 1 delivers a Go binary that:

1. Wraps invocations of the `claude` and `codex` CLIs in a PTY.
2. Records each invocation as a `session` row in a local SQLite database.
3. Captures the raw terminal byte stream and the user's keystroke stdin stream as time/size-chunked events plus on-disk artifacts.
4. Records cheap git metadata at session boundaries.
5. Provides minimal `sessions` and `show` subcommands to prove the recorder works.

Anything **not** explicitly listed in §1 of this document is **out of scope** for Phase 1 and must not be implemented, even partially, except for the no-op `Redactor` interface in §6.4 which is a forward-compat seam.

---

## 1. Phase 1 in-scope feature list (the only things to build)

| # | Feature | Source in GOAL.md |
|---|---------|-------------------|
| 1 | `agentrun claude [args...]` — PTY-wraps `claude` binary, passes args verbatim | §10, §14.1 |
| 2 | `agentrun codex [args...]` — PTY-wraps `codex` binary, passes args verbatim | §10, §14.1 |
| 3 | Per-invocation `sessions` row (start + end + exit_code) | §9.1, §14.1 |
| 4 | `terminal.output` events (PTY merged stdout/stderr), chunked, with on-disk `terminal_log` artifact | §6.1, §9.2 |
| 5 | `terminal.stdin` events (raw keystrokes), chunked, with on-disk artifact | §6.1, §9.2 |
| 6 | `AGENTRUN_SESSION_ID` env var injected into child | §16.5 |
| 7 | SIGWINCH propagation (host resize -> child PTY resize) | §6.1 |
| 8 | SIGINT/SIGTERM forwarded to child; session row finalized in deferred shutdown | §9.1 |
| 9 | Cheap git metadata at session boundaries: `repo_root`, `branch`, `start_commit_sha`, `end_commit_sha` | §9.1, §6.4 (minimal subset) |
| 10 | `session.started` and `session.ended` events with `source='system'` | §7.2 |
| 11 | `agentrun sessions` (table list) and `agentrun show <session_id>` (basic summary) | §9.7, §10 |
| 12 | Single static binary via `go build ./cmd/agentrun` (no CGO) | user constraint |

Explicit non-goals (do NOT touch in Phase 1): hooks/HTTP receiver, fsnotify, validation classifier, JSONL export, real regex redaction, web UI, git diff capture.

---

## 2. Locked design decisions

The following are decided. The implementer does not re-evaluate them.

### 2.1 CLI framework
**Decision:** stdlib `flag` plus a hand-rolled subcommand dispatcher in `internal/cli/root.go`.
**Reason:** Four subcommands with passthrough args; cobra adds 1.5 MB and dependency surface without earning its keep at this scale.

### 2.2 SQLite driver
**Decision:** `modernc.org/sqlite` (pure Go, no CGO).
**Reason:** Single static binary requirement. Caveat to note in code comments: it is ~2-4x slower than `mattn/go-sqlite3` on bulk inserts; we mitigate via batched transactions (§6.3). Acceptable at expected event rates (<1000 events/sec).

### 2.3 ID format
**Decision:** ULID via `github.com/oklog/ulid/v2`. Prefixes:
- Sessions: `s_<26-char-ulid>` (e.g. `s_01JBQK9XYZAB...`)
- Events: `evt_<26-char-ulid>`
- Artifacts: `art_<26-char-ulid>`

ULIDs are lexicographically sortable, which we exploit for default ordering in `sessions` listing.

### 2.4 PTY library
**Decision:** `github.com/creack/pty` v1.1.x.
**Reason:** De-facto standard, supports `Setsize`, `Start`, and exposes the master `*os.File`.
**Cross-platform notes:**
- macOS uses `/dev/ptmx` BSD-style; `creack/pty` wraps `posix_openpt`.
- Linux uses `/dev/ptmx` Linux-style with `grantpt`/`unlockpt`.
- Windows is not supported in Phase 1 (build tag `//go:build !windows` on the PTY package). The `main.go` will print a clear error on Windows.

### 2.5 Event writer concurrency model
**Decision:** Single writer goroutine consuming a buffered channel of `event` structs.
- Channel buffer size: **1024 events**.
- Batch flush trigger: **every 250ms OR every 64 events**, whichever comes first.
- Each flush is one `BEGIN IMMEDIATE; INSERT...; COMMIT;` transaction.
- On shutdown: channel is closed, writer drains remaining items, then exits. Caller `Close()` blocks on `<-doneCh`.

### 2.6 Terminal chunking thresholds
**Decision:**
- **Time window:** 50ms (flush partial chunk after 50ms of no further bytes on that stream).
- **Byte threshold:** 4096 bytes (flush immediately when buffer reaches 4 KiB).
- Each chunk becomes one `terminal.output` or `terminal.stdin` event with a `bytes` field (base64-encoded raw bytes — preserves binary control sequences exactly).

### 2.7 Child-not-found behavior
**Decision:** If `exec.LookPath("claude")` or `exec.LookPath("codex")` fails, print `agentrun: <agent> not found on PATH` to stderr and `os.Exit(127)`. **No session row is created.** No DB connection is opened.

### 2.8 Wrapper crash / channel drain semantics
**Decision:**
- Recorder uses `defer recorder.Close()` in `runAgent`. `Close()` closes the input channel, waits up to 2s for the writer goroutine to drain, then forcibly returns (logs `agentrun: recorder shutdown timed out, N events lost`).
- The `session.ended` event is emitted **synchronously** (bypassing the channel) so it is always written even if the channel is congested.
- The session row's `ended_at` and `exit_code` are also written synchronously via a direct `UPDATE sessions SET ...` in `Close()`.
- On panic in the writer goroutine, the deferred recover logs to stderr and the program continues to exit cleanly; events buffered in the channel are lost but the session row is still finalized.

### 2.9 Artifact disk layout
**Decision:**
- DB dir: `<repo_root>/.agentrun/` if `git rev-parse --show-toplevel` succeeds in cwd, else `$HOME/.agentrun/`. Overridable via env var `AGENTRUN_DB_DIR`.
- DB file: `<db_dir>/agentrun.db`.
- Artifacts: `<db_dir>/artifacts/<session_id>/pty.raw` and `<db_dir>/artifacts/<session_id>/stdin.raw`.
- Files are written via `io.MultiWriter`-style tee from the PTY copy loops in real time (not at shutdown).
- One `artifacts` row per file, inserted at `session.started` time (we know the path up front; `size_bytes` and `content_hash` are filled in at session end).

### 2.10 Stdin capture caveat (document, do not solve)
**Decision:** The host terminal is put into raw mode for the duration of the child run, so stdin arrives as individual keystrokes (including arrow keys, escape sequences, paste blobs). Phase 1 records these raw bytes verbatim. Translating keystrokes to logical "user prompts" is Phase 2's job (via Claude/Codex hooks). A comment in `internal/pty/pty.go` will state this explicitly.

---

## 3. Repository layout

```
/
├── GOAL.md                            (existing, unchanged)
├── PHASE1_PLAN.md                     (this file)
├── go.mod
├── go.sum
├── README.md                          (NOT in scope; do not create)
├── cmd/
│   └── agentrun/
│       └── main.go                    (entrypoint, ~30 lines)
├── internal/
│   ├── cli/
│   │   ├── root.go                    (subcommand dispatch)
│   │   ├── claude.go                  (`agentrun claude` handler)
│   │   ├── codex.go                   (`agentrun codex` handler)
│   │   ├── sessions.go                (`agentrun sessions` handler)
│   │   └── show.go                    (`agentrun show <id>` handler)
│   ├── agent/
│   │   └── agent.go                   (PATH resolution, version detect)
│   ├── config/
│   │   └── config.go                  (env vars, default paths)
│   ├── db/
│   │   ├── db.go                      (Open, Migrate, Close)
│   │   └── queries.go                 (typed insert/update/select)
│   ├── gitmeta/
│   │   └── gitmeta.go                 (4 cheap shell-outs)
│   ├── ids/
│   │   └── ids.go                     (ULID wrappers)
│   ├── pty/
│   │   └── pty.go                     (PTY alloc, raw mode, SIGWINCH, copy)
│   ├── recorder/
│   │   ├── recorder.go                (lifecycle, event channel, writer loop)
│   │   ├── chunker.go                 (time/size chunking for streams)
│   │   └── redactor.go                (Redactor interface + NoopRedactor)
│   └── schema/
│       ├── schema.go                  (//go:embed)
│       └── schema.sql                 (the DDL from §4)
```

Note: `internal/schema/` (not `schema/` at repo root) because Go's `//go:embed` is cleaner when the SQL lives next to the Go file that embeds it. The user's prompt suggested `schema/schema.sql`; this minor relocation is justified and noted here so the implementer doesn't second-guess.

---

## 4. Database schema

Write this verbatim to `internal/schema/schema.sql`. Do not modify column names, types, or constraints.

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous  = NORMAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

CREATE TABLE IF NOT EXISTS sessions (
  id                TEXT PRIMARY KEY,
  agent             TEXT NOT NULL,
  agent_version     TEXT,
  model             TEXT,
  permission_mode   TEXT,
  cwd               TEXT NOT NULL,
  repo_root         TEXT,
  branch            TEXT,
  start_commit_sha  TEXT,
  end_commit_sha    TEXT,
  started_at        TEXT NOT NULL,
  ended_at          TEXT,
  exit_code         INTEGER,
  transcript_path   TEXT,
  pid               INTEGER,
  metadata_json     TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata_json))
);

CREATE TABLE IF NOT EXISTS events (
  id              TEXT PRIMARY KEY,
  session_id      TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  sequence        INTEGER NOT NULL,
  ts              TEXT NOT NULL,
  source          TEXT NOT NULL,
  type            TEXT NOT NULL,
  payload_json    TEXT NOT NULL CHECK(json_valid(payload_json)),
  redaction_version TEXT,
  created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  UNIQUE (session_id, sequence)
);
CREATE INDEX IF NOT EXISTS events_session_ts   ON events(session_id, ts);
CREATE INDEX IF NOT EXISTS events_session_type ON events(session_id, type);
CREATE INDEX IF NOT EXISTS events_type_ts      ON events(type, ts);

CREATE TABLE IF NOT EXISTS artifacts (
  id             TEXT PRIMARY KEY,
  session_id     TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  event_id       TEXT REFERENCES events(id),
  kind           TEXT NOT NULL,
  path           TEXT,
  content_hash   TEXT,
  size_bytes     INTEGER,
  mime           TEXT,
  metadata_json  TEXT NOT NULL DEFAULT '{}' CHECK(json_valid(metadata_json)),
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX IF NOT EXISTS artifacts_session ON artifacts(session_id);
CREATE INDEX IF NOT EXISTS artifacts_hash    ON artifacts(content_hash);

CREATE TABLE IF NOT EXISTS session_summary (
  session_id        TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  user_prompts      INTEGER NOT NULL DEFAULT 0,
  tool_calls        INTEGER NOT NULL DEFAULT 0,
  files_changed     INTEGER NOT NULL DEFAULT 0,
  commands_run      INTEGER NOT NULL DEFAULT 0,
  validations_run   INTEGER NOT NULL DEFAULT 0,
  validations_pass  INTEGER NOT NULL DEFAULT 0,
  validations_fail  INTEGER NOT NULL DEFAULT 0,
  approvals_request INTEGER NOT NULL DEFAULT 0,
  approvals_denied  INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  status            TEXT
);
```

Phase 1 writes to `sessions`, `events`, `artifacts`, and `session_summary` (only the `status` column, set to `running` at start and `completed`/`failed` at end based on exit code). The other counters stay at 0 until Phase 2+.

---

## 5. Dependencies (go.mod)

| Module | Version pin policy | Purpose | License |
|--------|-------------------|---------|---------|
| `modernc.org/sqlite` | latest minor (`v1.34.x` as of 2026-05) | Pure-Go SQLite driver (no CGO) | BSD-3-Clause |
| `github.com/creack/pty` | latest minor (`v1.1.x`) | PTY allocation, SIGWINCH `Setsize` | MIT |
| `github.com/oklog/ulid/v2` | latest minor (`v2.1.x`) | ULID generation | Apache-2.0 |
| `golang.org/x/term` | latest minor | Raw mode toggle for host terminal stdin | BSD-3-Clause |
| `golang.org/x/sys` | latest minor (transitive of `x/term` and `pty`) | syscall helpers | BSD-3-Clause |

No other direct deps. Specifically: no cobra, no logrus, no go-git, no fsnotify, no yaml, no testify (use stdlib `testing`).

`go.mod` skeleton:
```
module github.com/<owner>/agentrun

go 1.22

require (
    github.com/creack/pty v1.1.21
    github.com/oklog/ulid/v2 v2.1.0
    golang.org/x/term v0.20.0
    modernc.org/sqlite v1.30.0
)
```
Replace `<owner>` with the actual GitHub owner at module-init time. Run `go mod tidy` to lock indirect deps and write `go.sum`.

---

## 6. Detailed file specifications

### 6.1 `cmd/agentrun/main.go`

**Purpose:** Single entrypoint. Delegates to `internal/cli.Run(args)`.

```go
package main

import (
    "fmt"
    "os"

    "github.com/<owner>/agentrun/internal/cli"
)

func main() {
    if err := cli.Run(os.Args[1:]); err != nil {
        fmt.Fprintf(os.Stderr, "agentrun: %v\n", err)
        // exit codes: 1 = generic error, 2 = usage error, 127 = agent not found,
        // otherwise child exit code is propagated by cli.Run via os.Exit inside the handler
        os.Exit(1)
    }
}
```

Note: handlers that wrap a child agent call `os.Exit(childExitCode)` themselves so the wrapper is transparent. `main` only handles the case where the CLI layer returned a Go error.

---

### 6.2 `internal/cli/root.go`

**Purpose:** Subcommand dispatch and shared usage text.

```go
package cli

import (
    "errors"
    "fmt"
)

var ErrUsage = errors.New("usage")

// Run executes the CLI with the given args (already stripped of argv[0]).
func Run(args []string) error

// usage prints help text to stderr and returns ErrUsage.
func usage() error
```

Dispatch table (switch on `args[0]`):
- `"claude"` -> `runClaude(args[1:])`
- `"codex"` -> `runCodex(args[1:])`
- `"sessions"` -> `runSessions(args[1:])`
- `"show"` -> `runShow(args[1:])`
- `"help"`, `"-h"`, `"--help"`, `""` -> `usage()`
- default -> `fmt.Errorf("unknown command: %q (try 'agentrun help')", args[0])`

Usage text:
```
agentrun — record local agent sessions

Usage:
  agentrun claude [args...]    Run the Claude Code CLI under recording
  agentrun codex  [args...]    Run the Codex CLI under recording
  agentrun sessions            List recorded sessions
  agentrun show <session_id>   Show summary for a session
  agentrun help                Show this help
```

---

### 6.3 `internal/cli/claude.go` and `internal/cli/codex.go`

Both files are near-identical thin wrappers over a shared `runAgent` helper. Put `runAgent` in `claude.go` (or a separate `agent_run.go` — implementer's call, but keep it adjacent).

```go
// runClaude wraps `claude` with the given passthrough args.
func runClaude(args []string) error

// runCodex wraps `codex` with the given passthrough args.
func runCodex(args []string) error

// runAgent is the shared wrapper logic.
//   agentName: "claude" or "codex" (stored in sessions.agent)
//   args:       passthrough args for the child process
// It NEVER returns nil on success — it calls os.Exit(childExitCode) so the wrapper is transparent.
// It returns an error only for setup failures (agent not on PATH, DB open failure, etc.).
func runAgent(agentName string, args []string) error
```

`runAgent` flow (this is the spine of Phase 1):

1. Resolve agent binary via `agent.Resolve(agentName)`. On failure: print `agentrun: <agentName> not found on PATH` and `os.Exit(127)`.
2. Detect agent version via `agent.Version(binaryPath)` (best-effort; ignore errors).
3. Load `config.Load()` -> resolves `dbDir`, `dbPath`, `artifactsDir`.
4. Open DB via `db.Open(dbPath)`. On failure: return wrapped error.
5. Gather git metadata via `gitmeta.Snapshot(cwd)` (best-effort; populate only what succeeds).
6. Create session row via `recorder.Start(...)` which:
   - generates `s_<ulid>`,
   - INSERTs `sessions` row with `started_at=now`, `pid=os.Getpid()` (will be updated after fork), `repo_root`, `branch`, `start_commit_sha`, `agent`, `agent_version`, `cwd`,
   - INSERTs `session_summary` row with `status='running'`,
   - creates `<artifactsDir>/<sessionID>/` directory,
   - INSERTs two `artifacts` rows for `pty.raw` and `stdin.raw` (kind=`terminal_log`),
   - starts the event-writer goroutine,
   - emits `session.started` event synchronously.
7. `defer recorder.Close(...)` — see step 12.
8. Build `exec.Cmd` for the agent: `cmd := exec.Command(binaryPath, args...)`; set `cmd.Env = append(os.Environ(), "AGENTRUN_SESSION_ID="+sessionID)`; set `cmd.Dir = cwd`.
9. Allocate PTY via `pty.Start(cmd, recorder)` which:
   - puts host terminal into raw mode (saving previous state),
   - calls `pty.Start(cmd)` (creack lib),
   - copies host stdin -> PTY master (teeing into recorder via the chunker),
   - copies PTY master -> host stdout (teeing into recorder via the chunker),
   - installs SIGWINCH handler that forwards `pty.InheritSize(os.Stdin, ptmx)` to the child,
   - returns a `*PtyHandle` with `Wait()` and child PID.
10. After PTY is up, `recorder.UpdatePID(childPID)` (`UPDATE sessions SET pid=?`).
11. Install signal handlers (SIGINT, SIGTERM): forward signal to `cmd.Process.Signal(sig)`.
12. Wait for child: `exitCode := ptyHandle.Wait()`.
13. The deferred `recorder.Close(endCommitSHA, exitCode)`:
    - emits `session.ended` event synchronously,
    - closes event channel and waits for drain (up to 2s),
    - fetches `gitmeta.HeadCommit(cwd)` for `end_commit_sha`,
    - finalizes artifact rows (`size_bytes`, `content_hash`),
    - `UPDATE sessions SET ended_at=?, exit_code=?, end_commit_sha=?`,
    - `UPDATE session_summary SET status=?` (`'completed'` if exit_code==0 else `'failed'`),
    - restores host terminal mode (must run even on panic — wrap in its own defer inside pty package).
14. `os.Exit(exitCode)`.

---

### 6.4 `internal/recorder/recorder.go`

```go
package recorder

import (
    "context"
    "database/sql"
    "sync"
    "time"
)

// Event is the in-memory representation of an event before insertion.
type Event struct {
    ID       string
    Sequence int64
    Ts       time.Time
    Source   string // "pty" | "system"
    Type     string // "terminal.output" | "terminal.stdin" | "session.started" | "session.ended"
    Payload  []byte // valid JSON
}

// Recorder owns the event channel, writer goroutine, and session lifecycle.
type Recorder struct {
    db         *sql.DB
    sessionID  string
    redactor   Redactor
    ch         chan Event
    seq        int64        // atomic counter; access via atomic.AddInt64
    wg         sync.WaitGroup
    closeOnce  sync.Once
    ptyArt     string       // artifact id for pty.raw
    stdinArt   string       // artifact id for stdin.raw
    ptyPath    string
    stdinPath  string
}

// Start creates the session, inserts the artifact rows, and spawns the writer goroutine.
func Start(db *sql.DB, opts StartOpts) (*Recorder, error)

// StartOpts groups the inputs for Start.
type StartOpts struct {
    Agent           string
    AgentVersion    string
    Cwd             string
    RepoRoot        string
    Branch          string
    StartCommitSHA  string
    ArtifactsDir    string  // <db_dir>/artifacts
    Redactor        Redactor
}

// SessionID returns the session's ULID-prefixed ID.
func (r *Recorder) SessionID() string

// PtyArtifactPath returns the absolute path the PTY copy goroutine should tee into.
func (r *Recorder) PtyArtifactPath() string

// StdinArtifactPath returns the absolute path the stdin copy goroutine should tee into.
func (r *Recorder) StdinArtifactPath() string

// Emit pushes an event onto the channel (non-blocking; drops with a stderr warning if full).
func (r *Recorder) Emit(source, eventType string, payload []byte)

// EmitSync inserts an event directly, bypassing the channel. Used for session.started/ended.
func (r *Recorder) EmitSync(source, eventType string, payload []byte) error

// UpdatePID updates sessions.pid after the child is forked.
func (r *Recorder) UpdatePID(pid int) error

// Close finalizes the session. Idempotent. endCommitSHA may be "" if not in a git repo.
func (r *Recorder) Close(endCommitSHA string, exitCode int) error
```

Writer goroutine (`run()`):
```
buf := make([]Event, 0, 64)
ticker := time.NewTicker(250 * time.Millisecond)
defer ticker.Stop()

flush := func() {
    if len(buf) == 0 { return }
    tx, _ := db.BeginTx(ctx, nil)
    stmt, _ := tx.Prepare(insertEventSQL)
    for _, e := range buf {
        stmt.Exec(e.ID, sessionID, e.Sequence, e.Ts.UTC().Format(time.RFC3339Nano), e.Source, e.Type, e.Payload, redactorVersion)
    }
    stmt.Close()
    tx.Commit()
    buf = buf[:0]
}

for {
    select {
    case e, ok := <-ch:
        if !ok { flush(); return }
        buf = append(buf, e)
        if len(buf) >= 64 { flush() }
    case <-ticker.C:
        flush()
    }
}
```

If `tx.Commit()` returns an error, log to stderr with `agentrun: event flush failed: <err>` but do not crash; subsequent batches will try again.

---

### 6.5 `internal/recorder/chunker.go`

A `*Chunker` wraps a tee-style `io.Writer`. The PTY copy loops write raw bytes to a chunker, which:
- accumulates into an internal buffer,
- flushes to the recorder (as one event) when either (a) buffer reaches 4096 bytes or (b) 50ms has passed since the first byte landed in the buffer with no further bytes.

```go
type Chunker struct {
    rec       *Recorder
    source    string
    eventType string
    buf       []byte
    mu        sync.Mutex
    timer     *time.Timer
}

// NewChunker returns a chunker that emits events of (source, eventType) to rec.
func NewChunker(rec *Recorder, source, eventType string) *Chunker

// Write satisfies io.Writer. It never blocks the caller for more than a buffer copy.
func (c *Chunker) Write(p []byte) (int, error)

// Flush emits any pending buffered bytes immediately.
func (c *Chunker) Flush()

// Close is Flush + stop timer.
func (c *Chunker) Close() error
```

Payload JSON shape for `terminal.output` and `terminal.stdin`:
```json
{"bytes_b64": "...base64...", "len": 1234}
```

Use `encoding/base64.StdEncoding`. Document in code: we use base64 (not raw JSON string) because PTY bytes routinely include control characters (`0x00`-`0x1f`, ESC sequences) that would require escaping in JSON anyway, and base64 keeps the JSON checker happy and round-trippable.

---

### 6.6 `internal/recorder/redactor.go`

```go
package recorder

// Redactor transforms an event payload before it is persisted.
// Phase 1 ships only NoopRedactor. Phase 2+ may swap in a regex-based implementation.
type Redactor interface {
    Redact(eventType string, payload []byte) (redacted []byte, version string)
}

// NoopRedactor returns payload unchanged. Its version is "noop-1".
type NoopRedactor struct{}

func (NoopRedactor) Redact(eventType string, payload []byte) ([]byte, string) {
    return payload, "noop-1"
}
```

The `version` is written to `events.redaction_version`. Future redactors should use semver-like strings (e.g. `"regex-1.0"`).

---

### 6.7 `internal/db/db.go`

```go
package db

import (
    "database/sql"
    _ "modernc.org/sqlite"
)

// Open opens (or creates) the SQLite database at path and applies the schema.
// Returns a *sql.DB with WAL mode already set via PRAGMA in schema.sql.
func Open(path string) (*sql.DB, error)

// Migrate executes the embedded schema.sql against db. Idempotent (uses IF NOT EXISTS).
func Migrate(db *sql.DB) error
```

Implementation notes:
- DSN: `file:<path>?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)`. Some PRAGMAs in the schema file may be redundant with the DSN — that's fine; PRAGMA execution is idempotent.
- Set `db.SetMaxOpenConns(1)` for the writer connection? **No** — single writer goroutine plus brief reads from CLI commands. Leave default. Document that all writes happen via the recorder goroutine.

---

### 6.8 `internal/db/queries.go`

Typed wrappers for every write/read in Phase 1. One function per query.

```go
// Sessions
func InsertSession(tx *sql.Tx, s SessionRow) error
func UpdateSessionPID(db *sql.DB, sessionID string, pid int) error
func FinalizeSession(db *sql.DB, sessionID, endCommit, status string, exitCode int, endedAt time.Time) error
func ListSessions(db *sql.DB, limit int) ([]SessionRow, error)
func GetSession(db *sql.DB, sessionID string) (SessionRow, error)

// Events
func InsertEventStmt(tx *sql.Tx) (*sql.Stmt, error)   // returns prepared INSERT for batch use
func InsertEventOne(db *sql.DB, e EventRow) error      // for EmitSync
func CountEventsByType(db *sql.DB, sessionID string) (map[string]int, error)

// Artifacts
func InsertArtifact(db *sql.DB, a ArtifactRow) error
func UpdateArtifactStats(db *sql.DB, artifactID string, sizeBytes int64, contentHash string) error

// session_summary
func InsertSessionSummary(db *sql.DB, sessionID, status string) error
func UpdateSessionSummaryStatus(db *sql.DB, sessionID, status string) error
```

Row structs (in the same file):
```go
type SessionRow struct {
    ID             string
    Agent          string
    AgentVersion   sql.NullString
    Model          sql.NullString
    PermissionMode sql.NullString
    Cwd            string
    RepoRoot       sql.NullString
    Branch         sql.NullString
    StartCommitSHA sql.NullString
    EndCommitSHA   sql.NullString
    StartedAt      time.Time
    EndedAt        sql.NullTime
    ExitCode       sql.NullInt64
    TranscriptPath sql.NullString
    PID            sql.NullInt64
    MetadataJSON   string
}

type EventRow struct {
    ID              string
    SessionID       string
    Sequence        int64
    Ts              time.Time
    Source          string
    Type            string
    PayloadJSON     []byte
    RedactionVersion sql.NullString
}

type ArtifactRow struct {
    ID           string
    SessionID    string
    EventID      sql.NullString
    Kind         string
    Path         sql.NullString
    ContentHash  sql.NullString
    SizeBytes    sql.NullInt64
    Mime         sql.NullString
    MetadataJSON string
}
```

Time format on the way to SQLite: `t.UTC().Format(time.RFC3339Nano)` for all `TEXT` timestamp columns. Reverse via `time.Parse(time.RFC3339Nano, s)`.

---

### 6.9 `internal/pty/pty.go`

Build tag: `//go:build !windows`.

```go
package pty

import (
    "io"
    "os"
    "os/exec"
)

// Handle is the running child + PTY pair.
type Handle struct {
    Ptmx *os.File
    Cmd  *exec.Cmd
    restoreTerm func() error
    sigwinchStop func()
}

// Start launches cmd attached to a new PTY.
// stdinTee, stdoutTee may be nil; if non-nil, copied bytes are also written there.
// The host terminal is put into raw mode; restore is wired into Handle.Close.
func Start(cmd *exec.Cmd, stdinTee, stdoutTee io.Writer) (*Handle, error)

// Wait blocks until the child exits and returns its exit code (or 1 on signal-kill).
// It is safe to call multiple times.
func (h *Handle) Wait() int

// Close restores the terminal and stops the SIGWINCH goroutine. Idempotent.
func (h *Handle) Close() error

// Signal forwards sig to the child process.
func (h *Handle) Signal(sig os.Signal) error
```

Implementation contract:
1. Call `pty.Start(cmd)` from `github.com/creack/pty`.
2. `term.MakeRaw(int(os.Stdin.Fd()))` to put host into raw mode; defer-restore via the stored `restoreTerm` closure.
3. Call `pty.InheritSize(os.Stdin, ptmx)` once at startup.
4. Spawn a goroutine listening on `signal.Notify(ch, syscall.SIGWINCH)` that calls `pty.InheritSize(os.Stdin, ptmx)` on each notification.
5. Spawn `go io.Copy(io.MultiWriter(ptmx, stdinTee), os.Stdin)` (stdin -> child + tee). If `stdinTee` is nil, just `io.Copy(ptmx, os.Stdin)`.
6. Spawn `go io.Copy(io.MultiWriter(os.Stdout, stdoutTee), ptmx)` (child -> stdout + tee).
7. `Wait()` calls `cmd.Wait()` and returns `cmd.ProcessState.ExitCode()`. If signaled, returns 128+signum (POSIX convention).

**Caveat to document in a comment block at the top of this file:**
> PTY merges stdout and stderr into a single byte stream — this is a fundamental property of pseudo-terminals, not a design choice of this recorder. Events sourced from this stream are typed `terminal.output`, not separate `terminal.stdout` / `terminal.stderr`. To distinguish the two, Phase 2+ must rely on hooks or invoke the child outside a PTY (which would break the TUI). Stdin is captured at the keystroke level because the host terminal is in raw mode; high-level prompt extraction is Phase 2's job.

---

### 6.10 `internal/agent/agent.go`

```go
package agent

import (
    "context"
    "errors"
    "os/exec"
    "strings"
    "time"
)

// ErrNotFound is returned when the agent binary is not on PATH.
var ErrNotFound = errors.New("not found on PATH")

// Resolve looks up the agent binary on PATH. agentName is "claude" or "codex".
// Returns the absolute path or wraps ErrNotFound.
func Resolve(agentName string) (string, error)

// Version invokes "<binaryPath> --version" with a 2s timeout and returns the trimmed stdout.
// Returns ("", nil) on any failure; never an error.
func Version(binaryPath string) string
```

`Resolve` is just `exec.LookPath(agentName)` with error wrapping.

`Version` runs the child with a `context.WithTimeout(ctx, 2*time.Second)`-derived `exec.CommandContext`, captures combined output, and trims whitespace. If the binary lacks `--version` or errors out, we return `""` — version is best-effort metadata.

---

### 6.11 `internal/gitmeta/gitmeta.go`

```go
package gitmeta

import "context"

// Snapshot bundles the cheap git facts we want at session boundaries.
type Snapshot struct {
    RepoRoot  string // "" if not in a repo
    Branch    string // "" if detached or not in a repo
    HeadSHA   string // "" if no commits or not in a repo
}

// Capture runs git rev-parse / git symbolic-ref to populate a Snapshot.
// Each shell-out has a 1s timeout. Never returns an error — partial results are OK.
func Capture(cwd string) Snapshot

// HeadCommit returns just the HEAD SHA (for end-of-session capture).
func HeadCommit(cwd string) string
```

Commands used (all run with `cwd` as working directory, 1s timeout each):
- `git rev-parse --show-toplevel` -> `RepoRoot`
- `git rev-parse --abbrev-ref HEAD` -> `Branch` (if output is `HEAD` literal, we set `Branch=""`)
- `git rev-parse HEAD` -> `HeadSHA`

If `git` is not on PATH, `Capture` returns an empty `Snapshot`. Document in code comments that this is intentional per GOAL.md §9.4 ("does not fail the whole session when outside a Git repo").

---

### 6.12 `internal/ids/ids.go`

```go
package ids

import (
    "crypto/rand"
    "github.com/oklog/ulid/v2"
    "time"
)

// New returns a fresh ULID string (26 chars, no prefix).
func New() string

// Session returns "s_<ulid>".
func Session() string

// Event returns "evt_<ulid>".
func Event() string

// Artifact returns "art_<ulid>".
func Artifact() string
```

Use a `ulid.MonotonicEntropy` reader wrapping `crypto/rand.Reader` (sync-protected) so two ULIDs generated in the same millisecond still sort correctly. Standard pattern:
```go
var entropy = ulid.Monotonic(rand.Reader, 0)
var entropyMu sync.Mutex
```

---

### 6.13 `internal/config/config.go`

```go
package config

// Config is the resolved runtime configuration.
type Config struct {
    DBDir        string  // absolute
    DBPath       string  // <DBDir>/agentrun.db
    ArtifactsDir string  // <DBDir>/artifacts
}

// Load resolves config from env + cwd. It does NOT create directories;
// callers (recorder.Start, db.Open) create them lazily.
func Load() (Config, error)
```

Resolution order for `DBDir`:
1. If `AGENTRUN_DB_DIR` is set in env, use it verbatim (after `filepath.Abs`).
2. Else, run `git rev-parse --show-toplevel` in cwd; if it succeeds, use `<repo_root>/.agentrun`.
3. Else, use `<$HOME>/.agentrun`. If `$HOME` is empty (rare), fall back to `os.TempDir()+"/agentrun"` and warn to stderr.

Document: only `AGENTRUN_DB_DIR` is honored in Phase 1. A future `~/.config/agentrun/config.yaml` is Phase 6+.

---

### 6.14 `internal/cli/sessions.go`

```go
func runSessions(args []string) error
```

Behavior:
- No flags in Phase 1.
- Open DB read-only.
- `db.ListSessions(db, 50)` -> print as a fixed-width table:
  ```
  SESSION ID                    AGENT    REPO                    STARTED                 STATUS
  s_01JBQK9XYZAB...             claude   /Users/x/app            2026-05-23 10:41:10     completed
  ```
- Columns truncated to: SESSION ID 30, AGENT 8, REPO 28, STARTED 20, STATUS 12.
- Sort by `started_at DESC`.
- Status is read from `session_summary.status`.

---

### 6.15 `internal/cli/show.go`

```go
func runShow(args []string) error
```

Behavior:
- Requires exactly one positional arg (session id). Else return `ErrUsage`.
- Open DB read-only.
- `db.GetSession(db, sessionID)` -> error if not found.
- `db.CountEventsByType(db, sessionID)` -> map for the summary block.
- Print:
  ```
  Session: s_01JBQK9XYZAB...
  Agent: claude (v0.1.2)
  Repo: /Users/x/app
  Branch: main
  Start commit: abc123de
  End commit:   def456ab
  Started: 2026-05-23T10:41:10Z
  Ended:   2026-05-23T10:53:22Z
  Exit code: 0
  Status: completed

  Events:
    terminal.output : 1432
    terminal.stdin  : 89
    session.started : 1
    session.ended   : 1

  Artifacts:
    terminal_log  pty.raw    312 KB
    terminal_log  stdin.raw    4 KB
  ```
- Truncate `End commit` to first 8 chars; show `(none)` if NULL.
- Use `text/tabwriter` for the alignment in the Events and Artifacts blocks.

---

## 7. Naming conventions (observed pattern targets)

Since the repo is greenfield, we set the conventions; the implementer should follow them consistently:

- **Package names:** lowercase, single word, no underscores. (`cli`, `recorder`, `db`, `pty`, `gitmeta`, `ids`, `config`, `agent`, `schema`.)
- **File names:** lowercase with underscores only if needed for disambiguation; prefer one concept per file (e.g. `chunker.go`, `redactor.go`).
- **Exported identifiers:** PascalCase. Unexported: lowerCamelCase.
- **Struct fields representing DB columns:** PascalCase Go names mapping to snake_case columns (e.g. `StartCommitSHA` -> `start_commit_sha`). Always document the mapping in a comment above the struct.
- **Error sentinels:** `ErrFoo` (e.g. `agent.ErrNotFound`).
- **Constructors:** `New<Type>` for trivial constructors; `Open` / `Start` / `Load` for ones with side effects.
- **JSON payloads in events:** snake_case keys (matches GOAL.md §11 example).
- **Timestamps in DB:** RFC3339Nano UTC strings.
- **Test files:** `<file>_test.go` in the same package.

---

## 8. Test strategy

### 8.1 Unit tests (write these in Phase 1)

| Package | Test file | What it tests |
|---------|-----------|---------------|
| `internal/ids` | `ids_test.go` | `Session()`, `Event()`, `Artifact()` produce correctly prefixed, monotonic, unique IDs. Test parallel generation (1000 goroutines x 100 IDs each, assert no duplicates). |
| `internal/db` | `db_test.go` | `Open` creates schema. `InsertSession` + `GetSession` round-trip. `InsertEventStmt` batch insert preserves order. `ListSessions` orders by `started_at DESC`. Foreign keys enforced (insert event without session -> error). Uses `t.TempDir()`-backed DB. |
| `internal/recorder` | `chunker_test.go` | Chunker flushes at byte threshold. Chunker flushes after time window with no further input. Chunker.Close flushes remaining bytes. Bytes through chunker exactly match input (no loss, no duplication). |
| `internal/recorder` | `recorder_test.go` | `Start` inserts session row + summary row + artifact rows. `Emit` events end up in DB after `Close`. `EmitSync` events are present even if channel was full. `Close` is idempotent. `Close` updates session row with exit_code and ended_at. |
| `internal/recorder` | `redactor_test.go` | `NoopRedactor` returns input bytewise-equal and version `"noop-1"`. |
| `internal/gitmeta` | `gitmeta_test.go` | In a `t.TempDir()` initialized with `git init` and one commit: `Capture` returns the repo root, branch, and SHA. Outside any repo: `Capture` returns empty struct, no error. `HeadCommit` returns the SHA. |
| `internal/agent` | `agent_test.go` | `Resolve("nonexistent_xyz")` returns `ErrNotFound`. `Resolve("sh")` returns an absolute path (uses `sh` because it's POSIX-guaranteed). `Version` on a fake script that prints `"v1.2.3"` returns `"v1.2.3"`. `Version` on `/bin/false` returns `""` without error. |
| `internal/config` | `config_test.go` | With `AGENTRUN_DB_DIR` env set, that value wins. Without it, in a temp git repo, returns `<repo>/.agentrun`. Outside any repo with `HOME` set, returns `<HOME>/.agentrun`. |

### 8.2 Integration tests (write one)

| Package | Test file | What it tests |
|---------|-----------|---------------|
| `internal/pty` (build-tag-guarded: `//go:build !windows`) | `pty_integration_test.go` | Spawns `/bin/cat` under `pty.Start` with a tee `bytes.Buffer`. Writes `"hello\n"` to the PTY master from the test. Sends EOF. Asserts the tee buffer contains `"hello"` (cat echoes raw lines; account for `\r\n` mapping in raw mode). Asserts `Wait()` returns 0. Runs only if `os.Getenv("AGENTRUN_PTY_TEST") == "1"` to avoid flakiness in environments without a usable PTY. |

End-to-end with `claude`/`codex` themselves is **manual** (§8.3).

### 8.3 Manual test checklist (run before declaring Phase 1 done)

1. `go build -o /tmp/agentrun ./cmd/agentrun` succeeds with no CGO (`CGO_ENABLED=0 go build ...` also succeeds).
2. `/tmp/agentrun help` prints usage.
3. `/tmp/agentrun claude` launches the real Claude Code TUI. Arrow keys, scrolling, color all work. Quit via Claude's quit command — wrapper exits with code 0.
4. After step 3: `/tmp/agentrun sessions` lists exactly one session with status `completed`.
5. `/tmp/agentrun show <id>` shows the session with at least one `terminal.output` event and at least one `terminal.stdin` event (from your keystrokes).
6. Inspect `.agentrun/artifacts/<id>/pty.raw` — should contain the TUI's raw byte stream (cat through `less -R` to view ANSI).
7. Inspect `.agentrun/artifacts/<id>/stdin.raw` — should contain your keystrokes.
8. Repeat steps 3-7 with `codex`.
9. Resize the host terminal during a Claude session — TUI re-flows correctly (SIGWINCH worked).
10. Hit Ctrl+C in Claude — Claude handles it (its own SIGINT handler runs); wrapper exits cleanly; session row has `exit_code` set.
11. Outside a git repo: `cd /tmp && /tmp/agentrun claude` — session created at `$HOME/.agentrun/agentrun.db`, no crash, `repo_root` is NULL in the row.
12. `claude` not on PATH: `PATH=/usr/bin /tmp/agentrun claude` -> stderr says `agentrun: claude not found on PATH`, exit 127, no session row created, no DB file created.

---

## 9. Acceptance checklist (mapped to GOAL.md §9.1 and §9.2)

### §9.1 Session Capture
| Item | Phase 1? | How |
|------|----------|-----|
| New session ID per invocation | **Yes** | `ids.Session()` |
| Session start and end persisted | **Yes** | `INSERT` at start, `UPDATE ended_at` at close |
| Agent name persisted | **Yes** | `sessions.agent` |
| Working directory persisted | **Yes** | `sessions.cwd` |
| Repo root detected when in a git repo | **Yes** | `gitmeta.Capture` |
| Start/end commit SHAs persisted | **Yes** | `gitmeta.Capture` + `gitmeta.HeadCommit` |

### §9.2 Terminal Capture
| Item | Phase 1? | How |
|------|----------|-----|
| stdout captured | **Yes** (as `terminal.output`) | PTY copy + chunker |
| stderr captured | **Yes** (merged into `terminal.output` — PTY behavior; documented) | PTY copy + chunker |
| stdin/user prompt captured | **Partial — raw keystrokes only** | PTY stdin tee + chunker. High-level prompt extraction deferred to Phase 2. |
| Process exit code captured | **Yes** | `cmd.ProcessState.ExitCode()` -> `sessions.exit_code` |
| Terminal capture associated with correct session ID | **Yes** | All events carry `session_id` FK |

### §9.3-9.7 — deferred to Phase 2-6 (NOT in Phase 1).

---

## 10. Phase 1-specific risks (top 3)

### R1: PTY raw-mode terminal restoration fails on abnormal exit
If the wrapper crashes after putting the host terminal in raw mode but before restoring it, the user is left with a borked shell (no echo, no line buffering). **Mitigation:** The `restoreTerm` closure is registered via `defer` inside `pty.Start` AND via a panic-recover wrapper in `runAgent`. Additionally, install a SIGSEGV / SIGABRT handler that calls `term.Restore` before re-raising. A `runAgent` panic must not leave the terminal broken — test this by deliberately `panic()`-ing during development.

### R2: modernc.org/sqlite write throughput under PTY firehose
A noisy TUI (Claude's spinner animations, syntax highlighting redraws) can emit 100+ KB/s of PTY output. At 4 KiB chunks, that's ~25 events/sec — well within batched-insert capability, but if the writer goroutine ever blocks on disk I/O for >250ms, the 1024-slot channel can fill. **Mitigation:** (a) channel `Emit` is non-blocking — if full, drop the event and log to stderr; (b) the on-disk `pty.raw` artifact is the canonical record, so dropping in-DB events still preserves the bytes; (c) the artifact write path is independent of the DB writer — even if SQLite is hung, the tee to disk continues.

### R3: SIGWINCH propagation race on startup
If the host terminal resizes between `pty.Start` and the SIGWINCH goroutine launching, the child PTY may have stale dimensions. **Mitigation:** Call `pty.InheritSize(os.Stdin, ptmx)` once unconditionally immediately after `pty.Start` returns, before starting copy goroutines. Document this ordering in code comments.

---

## 11. Implementation order

Build in this order. Each step ends with a working `go build` and (where applicable) passing tests.

1. **Skeleton.** Initialize the module: `go mod init github.com/<owner>/agentrun`. Create the directory tree from §3 with empty files. Make `cmd/agentrun/main.go` print `"agentrun: not yet implemented"`. `go build ./cmd/agentrun` must succeed.
2. **IDs.** Implement `internal/ids/ids.go` and its tests. Run `go test ./internal/ids/...`.
3. **Schema + DB.** Write `internal/schema/schema.sql` and `internal/schema/schema.go` (with `//go:embed`). Implement `internal/db/db.go` and `internal/db/queries.go`. Write unit tests against a `t.TempDir()` DB. Run `go test ./internal/db/...`.
4. **Config.** Implement `internal/config/config.go` and tests.
5. **Gitmeta.** Implement `internal/gitmeta/gitmeta.go` and tests (using `t.TempDir()` + `exec.Command("git", "init")`).
6. **Agent resolver.** Implement `internal/agent/agent.go` and tests.
7. **Redactor + Chunker.** Implement `internal/recorder/redactor.go` and `internal/recorder/chunker.go` with their tests. Chunker tests are the most important unit tests in Phase 1.
8. **Recorder core.** Implement `internal/recorder/recorder.go` — `Start`, `Emit`, `EmitSync`, writer goroutine, `Close`. Tests inject a mock or real DB and assert end-to-end event persistence.
9. **PTY package.** Implement `internal/pty/pty.go`. Hand-test against `/bin/cat` and `/bin/sh -i` before wiring into the CLI. Integration test (gated by env var).
10. **CLI: claude/codex.** Wire everything in `internal/cli/claude.go`, `internal/cli/codex.go`, and the shared `runAgent`. At this point `agentrun claude` must launch the real Claude Code TUI and produce a session row.
11. **CLI: sessions/show.** Implement `internal/cli/sessions.go` and `internal/cli/show.go`. These should be the last thing implemented because they're the proof-of-life that everything else worked.
12. **Manual test pass.** Run the full §8.3 checklist on macOS (primary) and Linux (secondary, via Docker or a remote box).
13. **Cleanup.** Run `go vet ./...`, `gofmt -s -w .`, ensure `go build -o /tmp/agentrun ./cmd/agentrun` produces a single static binary, `file /tmp/agentrun` confirms `Mach-O 64-bit executable` (macOS) or `ELF` (Linux) with no dynamic links to libsqlite.

---

## 12. Verification commands

The implementer runs these to confirm Phase 1 is done:

```bash
# Build
CGO_ENABLED=0 go build -o /tmp/agentrun ./cmd/agentrun

# Static linkage check (macOS)
otool -L /tmp/agentrun   # Should show only system libs, no libsqlite

# Static linkage check (Linux)
ldd /tmp/agentrun        # Should print "not a dynamic executable" or only system libs

# Tests
go test ./...
go vet ./...
gofmt -d .               # Must print nothing

# Smoke test (no real agent required)
ls -la
/tmp/agentrun help
/tmp/agentrun sessions   # Empty table on first run

# Real agent test (manual, requires claude on PATH)
/tmp/agentrun claude --version    # Should record a (very short) session, exit 0
/tmp/agentrun sessions            # Shows the session
/tmp/agentrun show s_<...>        # Shows summary with at least one terminal.output event

# Verify DB
sqlite3 .agentrun/agentrun.db 'SELECT id, agent, exit_code, status FROM sessions JOIN session_summary USING(session_id);'
```

Expected output of the final SQL query after one `claude --version` run:
```
s_01JBQK9XYZAB...|claude|0|completed
```

---

## 13. Out-of-scope reminder

If during implementation the implementer feels tempted to add any of the following, **stop and defer**:

- Any HTTP server.
- Any `fsnotify` import.
- Any regex over event payloads.
- Any `--format jsonl` flag on `agentrun show`.
- Any YAML or TOML config parsing.
- Any progress bars, spinners, or color in `sessions`/`show` output (plain text tables only).
- A `Dockerfile`, CI workflow, or release tooling.

Phase 1 is done when the §9 acceptance checklist items marked **Yes** all pass and the §8.3 manual checklist runs clean on macOS.
