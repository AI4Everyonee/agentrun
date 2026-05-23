# agentrun — Phase 2 Implementation Plan (Claude Code hooks only)

## 0. Scope statement

This document is the **only** input required to build Phase 2. Phase 1 (`PHASE1_PLAN.md`) is complete and green; this plan builds **on top** of that codebase.

Phase 2 delivers:

1. A new `agentrun hook <event-name>` subcommand that reads a single JSON object from stdin and writes one row to the `events` table.
2. Per-session injection of a generated Claude Code hooks settings file via `claude --settings <file>` so hooks fire for every Claude session wrapped by agentrun, without touching `~/.claude/settings.json`.
3. Cross-process event sequencing via SQL (`MAX(sequence)+1` in a transaction with retry), since hook invocations are separate processes from the PTY recorder.
4. Session-row enrichment from `SessionStart` payloads (`model`, `transcript_path`).
5. Atomic counter updates in `session_summary` for select hook events.
6. Concurrency-safe inserts (WAL + busy_timeout + retry-on-SQLITE_BUSY).
7. Manual + automated test coverage proving these work end-to-end against the real `claude` CLI.

**Explicit non-goals for Phase 2:**
- Codex hook integration (separate later phase).
- Copying the Claude transcript JSONL into our artifacts (we only store its path).
- A lookup table mapping Claude's `session_id` to our `s_<ulid>` (we store Claude's id in `payload_json`; ad-hoc queries suffice).
- HTTP-based hook receivers (we use `"type":"command"` only — simpler and faster).
- Reacting to hook contents to modify Claude's behavior (read-only).
- Hook redaction beyond the existing `NoopRedactor` (Phase 5+ owns real redaction).

---

## 1. Locked design decisions (the 12 questions, answered)

| # | Question | Decision | One-line reason |
|---|----------|----------|-----------------|
| 1 | Hook command argv shape | `agentrun hook <EventName>` (one positional arg) | Mirrors how Claude reuses argv per-event; simpler than parsing flags in the hot path; the `hook_event_name` from stdin is treated as authoritative if the two disagree (argv wins for routing, payload wins for storage). |
| 2 | Per-session hooks.json location | `<cfg.DBDir>/sessions/<sessionID>/hooks.json` | Lives next to other session-scoped derived state; easy to inspect post-mortem; cleaned up best-effort in `recorder.Close`. Not `os.TempDir()` because TempDir can be mounted noexec or differ across user/Claude env. |
| 3 | Path to `agentrun` binary in hook command | `os.Executable()` resolved at config-injection time, then `filepath.EvalSymlinks` to dereference | Most reliable; survives `$PATH` shenanigans; if `os.Executable` fails, we fall back to `exec.LookPath("agentrun")`, and if THAT fails, return an error and skip hook injection entirely (the session still records via PTY). |
| 4 | DB open mode in the hook subprocess | New `db.OpenReadWrite(path)` that skips migrations | Migrations are idempotent but execute many CREATE TABLE / PRAGMA statements; running them on every hook (10s–100s per session) adds avoidable latency to the hot path. Schema is guaranteed present because `runAgent` opened the DB with `db.Open` before spawning Claude. |
| 5 | Cleanup of per-session hooks.json | Best-effort `os.RemoveAll(<dbdir>/sessions/<sessionID>)` in `recorder.Close` | Tiny file; no harm if it leaks; we remove it for tidiness. Errors are logged to stderr but not propagated. |
| 6 | Missing `AGENTRUN_SESSION_ID` in hook env | Log one line to stderr (`agentrun hook: orphan invocation, AGENTRUN_SESSION_ID unset; ignoring`) and exit 0 | We must NEVER block Claude. An orphan is somebody calling our binary outside of a wrapped session. |
| 7 | Missing DB / unmigrated schema | Same as #6 — log to stderr, exit 0 | Read-only observability never crashes the agent. |
| 8 | Large payload cap | 256 KiB default; if exceeded, store `{"agentrun_truncated":true,"original_size":N,"truncated_payload_prefix_b64":"..."}` (the first 4 KiB base64-encoded) so the row is still useful. Configurable via `AGENTRUN_MAX_PAYLOAD_BYTES`. | Keeps SQLite row sizes sane; full payload still exists in Claude's transcript JSONL on disk. |
| 9 | SQLITE_BUSY retry | 3 attempts with backoffs **10ms, 50ms, 200ms** (each jittered ±25%). After 3 failures, log and drop. | Tail-loss is acceptable for observability; we have the transcript JSONL as backup. |
| 10 | UNIQUE-sequence-collision retry | Same envelope as #9 — 3 attempts. Each retry re-computes `MAX(sequence)+1` inside a fresh transaction. | Independent retries because the conflict comes from a peer process racing us, not from the lock. |
| 11 | Should hook return JSON on stdout | NO. Always print nothing to stdout. Exit 0. | Anything we write becomes a no-op directive to Claude; least surprise. |
| 12 | Hook registration scope | Per-session via `claude --settings <path>` only (NOT user or project scope) | Avoids leaking recording to every Claude invocation; respects the "wrapper opts you in" UX; `--setting-sources user,project,local` defaults remain on so the user's existing hooks still fire. |

---

## 2. New Claude CLI surface we depend on (treat as authoritative)

- `claude --settings <path-or-json>`: accepts a path to a JSON file OR an inline JSON string. We pass a path. Injected into argv BEFORE the user's args.
- `claude --setting-sources <user,project,local>`: we DO NOT pass this. Default behavior merges user + project + local with our `--settings` file layered on top, so user hooks still run.
- Hook subprocess env: inherits parent env. Our `AGENTRUN_SESSION_ID` and `AGENTRUN_DB_PATH` are automatically visible.
- Hook stdin payload: a single JSON object (NOT JSONL). Always includes:
  - `session_id` — Claude's internal ID (NOT our `s_<ulid>`; store verbatim in payload).
  - `transcript_path` — absolute path to the JSONL transcript on disk.
  - `cwd` — working directory at event time.
  - `hook_event_name` — case-sensitive event name (e.g. `PreToolUse`).
- Hook exit codes: `0` = success; `2` = blocking error (we NEVER return 2); other non-zero = non-blocking error (transcript shows `<hook> hook error`). Our hook MUST exit 0 on all paths.
- `claude --bare` skips all hooks. Documented limitation: `agentrun claude --bare` records PTY only, no hook events.
- `claude --debug=hooks`: useful for the manual test pass. The plan's manual checklist uses it.

---

## 3. MVP event set (the 11 we register in Phase 2)

| Claude event | Our `source` | Our `type` | Matcher value in hooks.json | `session_summary` counter |
|--------------|--------------|------------|-----------------------------|---------------------------|
| `SessionStart`        | `hook` | `hook.session_start`    | `""`           | (none) — also UPDATE sessions.model/transcript_path |
| `UserPromptSubmit`    | `hook` | `user.prompt`           | (no `matcher` field; event has no matcher) | `user_prompts += 1` |
| `PreToolUse`          | `hook` | `tool.pre_use`          | `""`           | `tool_calls += 1` |
| `PostToolUse`         | `hook` | `tool.post_use`         | `""`           | (none) |
| `PostToolUseFailure`  | `hook` | `tool.failed`           | `""`           | `errors += 1` |
| `PermissionRequest`   | `hook` | `approval.requested`    | `""`           | `approvals_request += 1` |
| `PermissionDenied`    | `hook` | `approval.responded`    | `""`           | `approvals_denied += 1` |
| `Notification`        | `hook` | `notification`          | `""`           | (none) |
| `Stop`                | `hook` | `response.stopped`      | (no `matcher`) | (none) |
| `PreCompact`          | `hook` | `context.pre_compact`   | `""`           | (none) |
| `SessionEnd`          | `hook` | `hook.session_end`      | `""`           | (none) |

**Unknown event handling:** If `agentrun hook <Foo>` is invoked with a name not in the table, persist the row with `source='hook'` and `type='hook.' + <Foo>` (case preserved). This is forward-compat for any new events Anthropic ships.

**Phase 3+ candidates (NOT registered now, do not implement):** SubagentStart, SubagentStop, TaskCreated, TaskCompleted, InstructionsLoaded, ConfigChange, CwdChanged, FileChanged, WorktreeCreate, WorktreeRemove, PostCompact, Elicitation, ElicitationResult, UserPromptExpansion, PostToolBatch, Setup, StopFailure, TeammateIdle.

---

## 4. Repository layout — new and modified files

### 4.1 New files

```
internal/
├── cli/
│   ├── hook.go                       (NEW — runHook subcommand)
│   └── hook_test.go                  (NEW — happy path + orphan path)
├── hooks/
│   ├── hooks.go                      (NEW — generate per-session settings JSON, cleanup)
│   ├── hooks_test.go                 (NEW — round-trip generation parsing)
│   ├── normalize.go                  (NEW — event-name → (source,type) + counter map)
│   └── normalize_test.go             (NEW — table-driven mapping test)
```

### 4.2 Modified files

```
internal/
├── cli/
│   ├── root.go                       (MOD — add "hook" case to dispatch)
│   └── agent_run.go                  (MOD — write hooks.json, inject --settings, inject env, cleanup)
├── db/
│   ├── db.go                         (MOD — add OpenReadWrite(path))
│   └── queries.go                    (MOD — add InsertEventWithAutoSeq, IncrementSummaryCounter,
│                                              UpdateSessionModel, UpdateSessionTranscriptPath)
└── recorder/
    └── recorder.go                   (MOD — best-effort delete <dbdir>/sessions/<sid>/ in Close)
```

No schema changes. No new Go dependencies. No package renames.

---

## 5. File-by-file specification

### 5.1 `internal/hooks/normalize.go` (NEW)

**Purpose:** Pure-function mapping from Claude event name to our normalized `(source, type)` and an associated `session_summary` counter (or empty string for "no counter").

```go
package hooks

// Mapping is the result of resolving one Claude hook event name.
type Mapping struct {
    Source      string // always "hook" today
    Type        string // e.g. "tool.pre_use"
    Counter     string // session_summary column name, or "" for no increment
    KnownEvent  bool   // true if this event is in our MVP table
}

// Normalize converts a Claude hook event name (e.g. "PreToolUse") into our
// canonical event-type taxonomy and the summary counter to increment.
//
// Unknown events return KnownEvent=false with Type="hook.<eventName>" and
// no counter. This is the forward-compat path for new Anthropic events.
func Normalize(eventName string) Mapping

// MVPEvents returns the list of Claude event names we register hooks for.
// Order is stable so unit tests can diff against it.
func MVPEvents() []string
```

**Implementation table (this exact set, in this exact order in `MVPEvents`):**

```go
var mvpTable = []struct {
    EventName string
    Type      string
    Counter   string
}{
    {"SessionStart",       "hook.session_start",  ""},
    {"UserPromptSubmit",   "user.prompt",         "user_prompts"},
    {"PreToolUse",         "tool.pre_use",        "tool_calls"},
    {"PostToolUse",        "tool.post_use",       ""},
    {"PostToolUseFailure", "tool.failed",         "errors"},
    {"PermissionRequest",  "approval.requested",  "approvals_request"},
    {"PermissionDenied",   "approval.responded",  "approvals_denied"},
    {"Notification",       "notification",        ""},
    {"Stop",               "response.stopped",    ""},
    {"PreCompact",         "context.pre_compact", ""},
    {"SessionEnd",         "hook.session_end",    ""},
}
```

`Normalize` does a linear scan (11 entries, cheaper than a map lookup at this size and avoids package-init allocation). Returns `Mapping{Source:"hook", Type:"hook."+eventName, KnownEvent:false}` on miss.

`MVPEvents` returns a freshly-allocated `[]string` of the event names in `mvpTable` order.

**Allowed counter values for `IncrementSummaryCounter` (validated in queries.go):** `user_prompts`, `tool_calls`, `errors`, `approvals_request`, `approvals_denied`. Any other value returns an error from `IncrementSummaryCounter` (defense against SQL injection via column-name interpolation).

---

### 5.2 `internal/hooks/hooks.go` (NEW)

**Purpose:** Generate the per-session `hooks.json` settings file that Claude will read via `--settings <path>`.

```go
package hooks

import (
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
)

// SessionDir returns the absolute directory path where per-session derived
// state lives. Layout: <dbDir>/sessions/<sessionID>/
func SessionDir(dbDir, sessionID string) string {
    return filepath.Join(dbDir, "sessions", sessionID)
}

// SettingsPath returns <SessionDir>/hooks.json.
func SettingsPath(dbDir, sessionID string) string {
    return filepath.Join(SessionDir(dbDir, sessionID), "hooks.json")
}

// GenerateSettings writes the per-session settings JSON to <SessionDir>/hooks.json.
// The file contains hook registrations for every MVPEvents() entry, each pointing
// to "<agentrunBinary> hook <EventName>".
//
//   dbDir          — agentrun's data directory (used to derive the session dir)
//   sessionID      — our s_<ulid>
//   agentrunBinary — absolute path to the running agentrun binary (from os.Executable)
//
// Returns the absolute path written. The directory is created with 0o755; the
// file is written with 0o644.
func GenerateSettings(dbDir, sessionID, agentrunBinary string) (string, error)

// Cleanup removes <SessionDir(dbDir, sessionID)>. Best-effort; errors are
// returned but the caller is expected to log-and-continue.
func Cleanup(dbDir, sessionID string) error
```

**`GenerateSettings` produced JSON shape** (write with `json.MarshalIndent(..., "", "  ")` for human-readability):

```json
{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook SessionStart", "timeout": 10 }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook UserPromptSubmit", "timeout": 10 }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook PreToolUse", "timeout": 10 }
        ]
      }
    ],
    "PostToolUse": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook PostToolUse", "timeout": 10 }
        ]
      }
    ],
    "PostToolUseFailure": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook PostToolUseFailure", "timeout": 10 }
        ]
      }
    ],
    "PermissionRequest": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook PermissionRequest", "timeout": 10 }
        ]
      }
    ],
    "PermissionDenied": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook PermissionDenied", "timeout": 10 }
        ]
      }
    ],
    "Notification": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook Notification", "timeout": 10 }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook Stop", "timeout": 10 }
        ]
      }
    ],
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook PreCompact", "timeout": 10 }
        ]
      }
    ],
    "SessionEnd": [
      {
        "matcher": "",
        "hooks": [
          { "type": "command", "command": "/abs/path/to/agentrun hook SessionEnd", "timeout": 10 }
        ]
      }
    ]
  }
}
```

**Rules for matcher emission (encoded in generator):**

- `UserPromptSubmit` and `Stop` are turn-level events with no tool matcher concept — emit a hook block WITHOUT a `"matcher"` field. (The JSON encoder must omit the field, not emit `"matcher": ""`. Use a struct with `Matcher *string` and set to nil for these two.)
- All other 9 events: emit `"matcher": ""` (empty string = catch-all).

**Internal types (kept package-private):**

```go
type hookCommand struct {
    Type    string `json:"type"`             // always "command"
    Command string `json:"command"`           // "<agentrun> hook <Event>"
    Timeout int    `json:"timeout,omitempty"` // seconds; 10 for all events
}

type hookGroup struct {
    Matcher *string       `json:"matcher,omitempty"` // nil => omitted; "" => emitted
    Hooks   []hookCommand `json:"hooks"`
}

type settingsFile struct {
    Hooks map[string][]hookGroup `json:"hooks"`
}
```

The command string is built with a single `fmt.Sprintf("%s hook %s", shellQuote(agentrunBinary), eventName)`. Use a tiny inline helper:

```go
// shellQuote wraps p in single quotes if it contains any character that
// would require escaping under sh -c. Safe for absolute paths produced by
// os.Executable. If p contains a single quote, we fall back to double
// quotes and escape inner double quotes; in practice agentrun's path
// won't have either.
func shellQuote(p string) string
```

Implementation: if `p` matches `^[A-Za-z0-9_/.@:+%~,=-]+$`, return as-is. Else wrap in single quotes with any inner `'` replaced by `'\''`.

**File creation:**

```go
func GenerateSettings(dbDir, sessionID, agentrunBinary string) (string, error) {
    dir := SessionDir(dbDir, sessionID)
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return "", fmt.Errorf("hooks.GenerateSettings: mkdir %q: %w", dir, err)
    }
    out := settingsFile{Hooks: map[string][]hookGroup{}}

    for _, ev := range MVPEvents() {
        empty := ""
        var matcher *string
        switch ev {
        case "UserPromptSubmit", "Stop":
            matcher = nil
        default:
            matcher = &empty
        }
        cmd := fmt.Sprintf("%s hook %s", shellQuote(agentrunBinary), ev)
        out.Hooks[ev] = []hookGroup{
            {Matcher: matcher, Hooks: []hookCommand{{Type: "command", Command: cmd, Timeout: 10}}},
        }
    }

    path := SettingsPath(dbDir, sessionID)
    b, err := json.MarshalIndent(out, "", "  ")
    if err != nil {
        return "", fmt.Errorf("hooks.GenerateSettings: marshal: %w", err)
    }
    if err := os.WriteFile(path, b, 0o644); err != nil {
        return "", fmt.Errorf("hooks.GenerateSettings: write %q: %w", path, err)
    }
    return path, nil
}
```

`Cleanup` is `os.RemoveAll(SessionDir(dbDir, sessionID))` wrapped with `fmt.Errorf` if it fails.

---

### 5.3 `internal/cli/hook.go` (NEW)

**Purpose:** Implement `agentrun hook <EventName>`. Reads JSON from stdin, persists one row, exits 0.

**Function signature & spine:**

```go
package cli

import (
    "encoding/base64"
    "encoding/json"
    "fmt"
    "io"
    "math/rand"
    "os"
    "strconv"
    "strings"
    "time"

    "github.com/jeevan/agentrun/internal/db"
    "github.com/jeevan/agentrun/internal/hooks"
    "github.com/jeevan/agentrun/internal/ids"
)

// defaultMaxPayloadBytes is the cap before truncation. Override via
// AGENTRUN_MAX_PAYLOAD_BYTES (integer, bytes).
const defaultMaxPayloadBytes = 256 * 1024

// runHook implements the `agentrun hook <EventName>` subcommand.
//
// Contract:
//   - args is the slice AFTER "hook" (so args[0] should be the event name).
//   - Reads a single JSON object from stdin (Claude delivers one per invocation).
//   - Persists one events row with source="hook" and a normalized type.
//   - ALWAYS exits 0 on the happy path AND on the orphan path. Returns nil so
//     root.go does not print a "agentrun: ..." error.
//   - Returns a non-nil error ONLY for a truly catastrophic, unrecoverable
//     issue (e.g. cli.ErrUsage if argv shape is wrong). main() will exit 1 in
//     that case; Claude treats non-zero as a non-blocking hook error.
//
// NB: We accept a non-zero exit ONLY for malformed argv (cli.ErrUsage path).
// All other failure modes log to stderr and return nil → exit 0.
func runHook(args []string) error
```

**Step-by-step flow** (implementer follows this exact order):

1. **Argv check.** If `len(args) != 1`, write `agentrun hook: usage: agentrun hook <EventName>` to stderr and return `nil` (exit 0). (Wrong argv from Claude shouldn't blow up the session.)
2. **Event name capture.** `argEventName := args[0]`.
3. **Env check.** Read `sessionID := os.Getenv("AGENTRUN_SESSION_ID")` and `dbPath := os.Getenv("AGENTRUN_DB_PATH")`. If EITHER is empty: `fmt.Fprintln(os.Stderr, "agentrun hook: orphan invocation (AGENTRUN_SESSION_ID or AGENTRUN_DB_PATH unset); ignoring")` and `return nil`. **Do not** read stdin (Claude won't care; we just exit silently).
4. **Read stdin.** `raw, err := io.ReadAll(os.Stdin)`. If err: `fmt.Fprintf(os.Stderr, "agentrun hook: read stdin: %v\n", err)`; `return nil`.
5. **Validate JSON.** `var probe map[string]any; if err := json.Unmarshal(raw, &probe); err != nil { ... }`. We don't fully decode — we just want to confirm it's valid JSON so the CHECK constraint won't fail. On invalid JSON, store a wrapping JSON instead:
   ```go
   raw = []byte(fmt.Sprintf(`{"agentrun_invalid_payload":true,"raw_b64":%q,"parse_error":%q}`,
       base64.StdEncoding.EncodeToString(raw), err.Error()))
   ```
6. **Truncate if too big.** Determine `maxBytes := defaultMaxPayloadBytes`. If `os.Getenv("AGENTRUN_MAX_PAYLOAD_BYTES") != ""`, parse it via `strconv.Atoi`; on parse error keep default. If `len(raw) > maxBytes`, replace with a truncation envelope:
   ```go
   prefix := raw[:4096] // first 4 KiB of the original bytes
   raw = []byte(fmt.Sprintf(`{"agentrun_truncated":true,"original_size":%d,"truncated_payload_prefix_b64":%q,"hook_event_name":%q}`,
       len(raw), base64.StdEncoding.EncodeToString(prefix), argEventName))
   ```
7. **Resolve mapping.** `m := hooks.Normalize(argEventName)`. We use `argEventName` (argv), not anything parsed from the payload, so routing is deterministic.
8. **Open DB read-write WITHOUT migrating.** `d, err := db.OpenReadWrite(dbPath)`. On error: log to stderr, return `nil`. `defer d.Close()`.
9. **Insert event with auto-sequence + retry.** Call:
   ```go
   _, err = db.InsertEventWithAutoSeq(d, sessionID, time.Now().UTC(), m.Source, m.Type, raw, "noop-1")
   if err != nil {
       fmt.Fprintf(os.Stderr, "agentrun hook: insert failed (event=%s session=%s): %v\n", argEventName, sessionID, err)
       return nil
   }
   ```
10. **Optional session enrichment (SessionStart only).** If `argEventName == "SessionStart"`:
    - Decode `raw` again into a struct `{Model string `json:"model"`; TranscriptPath string `json:"transcript_path"`}` (use `json.Unmarshal`; ignore errors — payload may not include these in every variant).
    - If `Model != ""`, call `db.UpdateSessionModel(d, sessionID, Model)`. Log-and-continue on error.
    - If `TranscriptPath != ""`, call `db.UpdateSessionTranscriptPath(d, sessionID, TranscriptPath)`. Log-and-continue on error.
11. **Optional session enrichment (any event).** If the payload's top-level `transcript_path` is non-empty AND the current row's `transcript_path` is NULL, update it. To avoid a SELECT every hook, just do `UPDATE sessions SET transcript_path = ? WHERE id = ? AND transcript_path IS NULL`. Best-effort; ignore rows-affected count.
    - Decision: do this step **only on SessionStart** to keep the hot path lean. The transcript path is stable per session; one update per session is enough.
12. **Counter increment.** If `m.Counter != ""`, call `db.IncrementSummaryCounter(d, sessionID, m.Counter)`. Log-and-continue on error.
13. **Return nil.** Exit 0.

**Performance notes baked into the implementation:**
- No unnecessary allocations on the happy path beyond `io.ReadAll` and one `json.Unmarshal` for the validity probe.
- `db.OpenReadWrite` skips schema execution; opening the DB is the dominant cost (~5-15ms on cold cache).
- We do NOT prepare statements (one-shot per process; preparing costs more than executing once).

---

### 5.4 `internal/cli/root.go` (MODIFY)

Add `"hook"` to the switch in `Run`. Also extend the help text.

**Location:** after the `case "show":` arm (current line ~26).

**Add:**
```go
case "hook":
    return runHook(args[1:])
```

**Help text** (replace the multi-line literal in `usage()`): keep the existing block but add one line BEFORE `agentrun help`:

```
  agentrun hook <event>        Internal: invoked by Claude hooks (do not call directly)
```

The line is intentionally documented as internal — users should not run it themselves.

---

### 5.5 `internal/cli/agent_run.go` (MODIFY)

Five surgical changes. All inside `runAgent`. Existing logic must remain intact.

#### Change A: Resolve `agentrun` absolute path (early, fail-soft)

**Location:** right after step 4 (`d, err := db.Open(cfg.DBPath)` and `defer d.Close()`), before step 5 (`gitmeta.Capture`).

**Imports to add:** `"path/filepath"` (likely already imported transitively; check and add if not).

**Code to insert:**
```go
// 4b. Resolve our own absolute path so the hooks settings file can name us.
//     Failure here is non-fatal: we just skip hooks and fall back to PTY-only recording.
agentrunBin := resolveSelfPath()
```

**Helper added at file scope (bottom of agent_run.go):**
```go
// resolveSelfPath returns the absolute path to the running agentrun binary.
// Tries os.Executable() first, then falls back to exec.LookPath("agentrun").
// Returns "" if both fail; callers must treat "" as "do not register hooks".
func resolveSelfPath() string {
    if p, err := os.Executable(); err == nil && p != "" {
        if abs, err := filepath.EvalSymlinks(p); err == nil {
            return abs
        }
        return p
    }
    if p, err := exec.LookPath("agentrun"); err == nil {
        return p
    }
    return ""
}
```

#### Change B: Generate hooks.json (Claude only)

**Location:** after step 6 (`rec, err := recorder.Start(...)`), before step 7 (artifact file open).

```go
// 6b. For Claude, generate the per-session hooks.json that Claude will read
//     via --settings. For Codex, skip — Codex hook support is a later phase.
var hooksSettingsPath string
if agentName == "claude" && agentrunBin != "" {
    p, err := hooks.GenerateSettings(cfg.DBDir, rec.SessionID(), agentrunBin)
    if err != nil {
        fmt.Fprintf(os.Stderr, "agentrun: warning: failed to write hooks settings (continuing without hooks): %v\n", err)
    } else {
        hooksSettingsPath = p
    }
}
```

**Imports to add** at the top of `agent_run.go`:
```go
"github.com/jeevan/agentrun/internal/hooks"
```

#### Change C: Prepend `--settings <path>` to args + inject env

**Location:** step 9 — modify the `exec.Command` construction.

**Current code:**
```go
cmd := exec.Command(binPath, args...)
cmd.Env = append(os.Environ(), "AGENTRUN_SESSION_ID="+rec.SessionID())
cmd.Dir = cwd
```

**Replace with:**
```go
childArgs := args
if hooksSettingsPath != "" {
    // Prepend --settings <path> so it takes precedence and isn't accidentally
    // overridden by something the user passed later. (Claude accepts the flag
    // anywhere on argv; prepending is just defensive.)
    childArgs = append([]string{"--settings", hooksSettingsPath}, args...)
}
cmd := exec.Command(binPath, childArgs...)
cmd.Env = append(os.Environ(),
    "AGENTRUN_SESSION_ID="+rec.SessionID(),
    "AGENTRUN_DB_PATH="+cfg.DBPath,
)
cmd.Dir = cwd
```

#### Change D: Pass sessionID + dbDir into recorder so Close can clean up

**Decision:** Keep `recorder.Close` signature unchanged. Cleanup is performed in `agent_run.go` directly after `rec.Close`. This avoids leaking knowledge of the hooks package into the recorder package.

**Location:** after step 17 (`handle.Close()`), before `os.Exit(exitCode)`.

```go
// 17b. Best-effort cleanup of the per-session hooks settings dir.
if hooksSettingsPath != "" {
    if err := hooks.Cleanup(cfg.DBDir, rec.SessionID()); err != nil {
        fmt.Fprintf(os.Stderr, "agentrun: warning: hooks cleanup: %v\n", err)
    }
}
```

#### Change E: (No change to recorder.go required after the decision above.)

The earlier draft had `recorder.Close` performing cleanup. The cleaner separation is to keep that in `agent_run.go`. Skip the `recorder/recorder.go` modification listed in §4.2 — **revise §4.2 accordingly:** no recorder changes are needed. The bullet under "Modified files" for `recorder/recorder.go` can be deleted by the implementer.

(Plan invariant: **do not modify `internal/recorder/recorder.go` in Phase 2.**)

---

### 5.6 `internal/db/db.go` (MODIFY)

Add ONE new function. No changes to existing code.

**Location:** end of file, after `Migrate`.

```go
// OpenReadWrite opens (but does NOT migrate) an existing SQLite database.
// Intended for short-lived processes — like `agentrun hook` — that run many
// times per session and must avoid the cost of re-executing schema.sql on
// every invocation.
//
// The caller is responsible for guaranteeing the DB exists and the schema is
// applied (this is true for any DB opened previously by db.Open). If the file
// is missing or the schema is absent, subsequent queries will fail loudly.
func OpenReadWrite(path string) (*sql.DB, error) {
    dsn := "file:" + path +
        "?_pragma=busy_timeout(5000)" +
        "&_pragma=journal_mode(WAL)" +
        "&_pragma=foreign_keys(on)" +
        "&_pragma=synchronous(NORMAL)"

    d, err := sql.Open("sqlite", dsn)
    if err != nil {
        return nil, fmt.Errorf("db.OpenReadWrite: sql.Open: %w", err)
    }
    // Single connection is fine; the hook process is short-lived.
    d.SetMaxOpenConns(1)
    return d, nil
}
```

---

### 5.7 `internal/db/queries.go` (MODIFY)

Add four new functions. Place them after `InsertEventOne` (around current line 309).

**Imports to add** (if not already present): `"errors"`, `"math/rand"`, `"strings"`, `"time"` (most are already imported).

#### A. `InsertEventWithAutoSeq`

```go
// InsertEventWithAutoSeq inserts one events row computing sequence as
// MAX(sequence)+1 inside a transaction. Designed for cross-process callers
// (hook subprocesses) that cannot share an in-memory atomic counter.
//
// On SQLITE_BUSY or a UNIQUE constraint violation (sequence collision with a
// concurrent writer), retries up to 3 times with jittered backoff
// (10ms, 50ms, 200ms ±25%). Returns the generated event ID on success.
//
// All errors after retries exhausted are returned to the caller; the caller
// is expected to log and continue (never block the parent agent).
func InsertEventWithAutoSeq(
    d *sql.DB,
    sessionID string,
    ts time.Time,
    source, eventType string,
    payload []byte,
    redactorVersion string,
) (eventID string, err error)
```

**Implementation:**

```go
var insertRetryBackoffs = []time.Duration{
    10 * time.Millisecond,
    50 * time.Millisecond,
    200 * time.Millisecond,
}

func InsertEventWithAutoSeq(
    d *sql.DB,
    sessionID string,
    ts time.Time,
    source, eventType string,
    payload []byte,
    redactorVersion string,
) (string, error) {
    var lastErr error
    for attempt := 0; attempt <= len(insertRetryBackoffs); attempt++ {
        evtID, err := tryInsertEventWithAutoSeq(d, sessionID, ts, source, eventType, payload, redactorVersion)
        if err == nil {
            return evtID, nil
        }
        lastErr = err
        if !isRetryableInsertErr(err) {
            return "", err
        }
        if attempt == len(insertRetryBackoffs) {
            break
        }
        sleepJittered(insertRetryBackoffs[attempt])
    }
    return "", fmt.Errorf("InsertEventWithAutoSeq: exhausted retries: %w", lastErr)
}

func tryInsertEventWithAutoSeq(
    d *sql.DB,
    sessionID string,
    ts time.Time,
    source, eventType string,
    payload []byte,
    redactorVersion string,
) (string, error) {
    tx, err := d.Begin()
    if err != nil {
        return "", fmt.Errorf("tryInsertEventWithAutoSeq: begin: %w", err)
    }
    defer func() {
        if err != nil {
            _ = tx.Rollback()
        }
    }()

    var nextSeq int64
    row := tx.QueryRow(
        `SELECT COALESCE(MAX(sequence), 0) + 1 FROM events WHERE session_id = ?`,
        sessionID,
    )
    if err = row.Scan(&nextSeq); err != nil {
        return "", fmt.Errorf("tryInsertEventWithAutoSeq: scan max(seq): %w", err)
    }

    evtID := ids.Event()
    _, err = tx.Exec(
        `INSERT INTO events (id, session_id, sequence, ts, source, type, payload_json, redaction_version)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
        evtID, sessionID, nextSeq,
        ts.UTC().Format(time.RFC3339Nano),
        source, eventType, payload, redactorVersion,
    )
    if err != nil {
        return "", err
    }

    if err = tx.Commit(); err != nil {
        return "", fmt.Errorf("tryInsertEventWithAutoSeq: commit: %w", err)
    }
    return evtID, nil
}

func isRetryableInsertErr(err error) bool {
    if err == nil {
        return false
    }
    msg := strings.ToLower(err.Error())
    // modernc.org/sqlite surfaces these strings; we match on substring so we
    // don't have to import sqlite3 error codes.
    return strings.Contains(msg, "sqlite_busy") ||
        strings.Contains(msg, "database is locked") ||
        strings.Contains(msg, "unique constraint failed") ||
        strings.Contains(msg, "constraint failed: events.session_id, events.sequence")
}

func sleepJittered(base time.Duration) {
    // ±25% jitter via math/rand. Seeded by package init in Go 1.20+ (auto).
    delta := time.Duration(rand.Int63n(int64(base) / 2))   // 0..base/2
    sign := time.Duration(1)
    if rand.Intn(2) == 0 {
        sign = -1
    }
    d := base + sign*(delta-base/4)
    if d < 0 {
        d = base
    }
    time.Sleep(d)
}
```

**Import requirement:** add `"math/rand"` and `"strings"` and `"github.com/jeevan/agentrun/internal/ids"` to `internal/db/queries.go`.

Note: importing `internal/ids` from `internal/db` is the first such dependency. There is no circular import risk (ids depends only on stdlib + ulid). Implementer should run `go vet ./...` to confirm.

#### B. `IncrementSummaryCounter`

```go
// allowedSummaryCounters lists session_summary columns safe to increment.
// We hard-code this to avoid SQL injection via interpolated column names.
var allowedSummaryCounters = map[string]bool{
    "user_prompts":      true,
    "tool_calls":        true,
    "errors":            true,
    "approvals_request": true,
    "approvals_denied":  true,
    "files_changed":     true, // reserved for Phase 4
    "commands_run":      true, // reserved for Phase 5
    "validations_run":   true, // reserved for Phase 5
    "validations_pass":  true, // reserved for Phase 5
    "validations_fail":  true, // reserved for Phase 5
}

// IncrementSummaryCounter atomically increments one counter column on the
// session_summary row for sessionID. Same retry envelope as InsertEventWithAutoSeq.
func IncrementSummaryCounter(d *sql.DB, sessionID, counter string) error {
    if !allowedSummaryCounters[counter] {
        return fmt.Errorf("IncrementSummaryCounter: counter %q not in allowlist", counter)
    }
    q := fmt.Sprintf(
        `UPDATE session_summary SET %s = %s + 1 WHERE session_id = ?`,
        counter, counter,
    )
    var lastErr error
    for attempt := 0; attempt <= len(insertRetryBackoffs); attempt++ {
        _, err := d.Exec(q, sessionID)
        if err == nil {
            return nil
        }
        lastErr = err
        if !isRetryableInsertErr(err) {
            return err
        }
        if attempt == len(insertRetryBackoffs) {
            break
        }
        sleepJittered(insertRetryBackoffs[attempt])
    }
    return fmt.Errorf("IncrementSummaryCounter: exhausted retries: %w", lastErr)
}
```

#### C. `UpdateSessionModel`

```go
// UpdateSessionModel sets sessions.model. Idempotent (overwrites). No-op on
// empty model. Same retry envelope.
func UpdateSessionModel(d *sql.DB, sessionID, model string) error {
    if model == "" {
        return nil
    }
    return execWithRetry(d, `UPDATE sessions SET model = ? WHERE id = ?`, model, sessionID)
}
```

#### D. `UpdateSessionTranscriptPath`

```go
// UpdateSessionTranscriptPath sets sessions.transcript_path ONLY IF the
// current value is NULL (so the first hook to report wins; later hooks
// are silently no-ops). Same retry envelope.
func UpdateSessionTranscriptPath(d *sql.DB, sessionID, path string) error {
    if path == "" {
        return nil
    }
    return execWithRetry(d,
        `UPDATE sessions SET transcript_path = ? WHERE id = ? AND transcript_path IS NULL`,
        path, sessionID,
    )
}
```

#### E. Shared retry helper

```go
// execWithRetry runs Exec with the standard retry envelope.
func execWithRetry(d *sql.DB, q string, args ...interface{}) error {
    var lastErr error
    for attempt := 0; attempt <= len(insertRetryBackoffs); attempt++ {
        _, err := d.Exec(q, args...)
        if err == nil {
            return nil
        }
        lastErr = err
        if !isRetryableInsertErr(err) {
            return err
        }
        if attempt == len(insertRetryBackoffs) {
            break
        }
        sleepJittered(insertRetryBackoffs[attempt])
    }
    return fmt.Errorf("execWithRetry: exhausted retries: %w", lastErr)
}
```

---

## 6. Naming conventions (must match Phase 1 exactly)

These mirror what's already in the codebase (verified by reading `internal/cli`, `internal/db`, `internal/recorder`):

- **Package names:** lowercase, single word. New package: `hooks`.
- **File names:** lowercase, one concept per file (`hook.go`, `normalize.go`, `hooks.go`).
- **Exported identifiers:** PascalCase. Unexported: lowerCamelCase.
- **Error wrapping:** `fmt.Errorf("Func: short reason: %w", err)` — the "Func: " prefix mirrors `db.queries.go` style (`InsertSession:`, `FinalizeSession:`).
- **Stderr logging from non-fatal paths:** `fmt.Fprintf(os.Stderr, "agentrun: ...\n", ...)` mirrors `agent_run.go` and `recorder.go`. Use `"agentrun hook: ..."` for messages from the hook subcommand specifically (so users can grep).
- **Event payload JSON keys:** snake_case (`hook_event_name`, `session_id`, `transcript_path`) — already established in GOAL.md §11 example.
- **Timestamp format:** `t.UTC().Format(time.RFC3339Nano)` — same as Phase 1.
- **DB column names:** snake_case (matches schema.sql).
- **Constants:** `defaultMaxPayloadBytes` lowercase camel because package-private (mirrors `recorder.batchSize`).
- **Counter column allowlist constant:** `allowedSummaryCounters` — package-private `map[string]bool`.
- **Test files:** `<file>_test.go` in same package (Phase 1 convention; no `_internal_` suffix).

---

## 7. Test plan

### 7.1 `internal/hooks/normalize_test.go`

Table-driven test. Single function `TestNormalize`. Cases:

| Input | Expected `Source` | Expected `Type` | Expected `Counter` | `KnownEvent` |
|-------|-------------------|-----------------|--------------------|--------------|
| `"SessionStart"`       | `"hook"` | `"hook.session_start"`     | `""`                  | true  |
| `"UserPromptSubmit"`   | `"hook"` | `"user.prompt"`            | `"user_prompts"`      | true  |
| `"PreToolUse"`         | `"hook"` | `"tool.pre_use"`           | `"tool_calls"`        | true  |
| `"PostToolUse"`        | `"hook"` | `"tool.post_use"`          | `""`                  | true  |
| `"PostToolUseFailure"` | `"hook"` | `"tool.failed"`            | `"errors"`            | true  |
| `"PermissionRequest"`  | `"hook"` | `"approval.requested"`     | `"approvals_request"` | true  |
| `"PermissionDenied"`   | `"hook"` | `"approval.responded"`     | `"approvals_denied"`  | true  |
| `"Notification"`       | `"hook"` | `"notification"`           | `""`                  | true  |
| `"Stop"`               | `"hook"` | `"response.stopped"`       | `""`                  | true  |
| `"PreCompact"`         | `"hook"` | `"context.pre_compact"`    | `""`                  | true  |
| `"SessionEnd"`         | `"hook"` | `"hook.session_end"`       | `""`                  | true  |
| `"SubagentStart"`      | `"hook"` | `"hook.SubagentStart"`     | `""`                  | false |
| `""`                   | `"hook"` | `"hook."`                  | `""`                  | false |

Also test `TestMVPEvents`: assert it returns exactly 11 entries, in the order matching the mvp table above.

### 7.2 `internal/hooks/hooks_test.go`

#### Test `TestGenerateSettings_Roundtrip`
- `tmpDir := t.TempDir()`
- Call `GenerateSettings(tmpDir, "s_test", "/usr/local/bin/agentrun")` → assert returned path equals `<tmpDir>/sessions/s_test/hooks.json`.
- Read the file back, `json.Unmarshal` into the public schema (write an inline struct mirroring `settingsFile`).
- Assert the top-level `Hooks` map has exactly 11 keys, matching `MVPEvents()`.
- Assert `Hooks["PreToolUse"][0].Hooks[0].Command == "/usr/local/bin/agentrun hook PreToolUse"`.
- Assert `Hooks["PreToolUse"][0].Matcher` is `*string` pointing to `""`.
- Assert `Hooks["UserPromptSubmit"][0].Matcher` is `nil` (NOT a pointer to ""). This requires the test to use a struct with `Matcher *string` and inspect the pointer directly — verifies omitempty correctness.
- Assert `Hooks["Stop"][0].Matcher` is `nil`.
- Assert `Hooks["PreToolUse"][0].Hooks[0].Timeout == 10`.

#### Test `TestGenerateSettings_ShellQuoting`
- Call `GenerateSettings(tmpDir, "s_quote", "/path with space/agentrun")`.
- Read file. Assert the resulting command contains a single-quoted absolute path: `'/path with space/agentrun' hook PreToolUse`.

#### Test `TestCleanup_RemovesDir`
- `GenerateSettings(tmpDir, "s_c", "/tmp/agentrun")` then `Cleanup(tmpDir, "s_c")`.
- Assert `<tmpDir>/sessions/s_c` does not exist.
- Calling Cleanup twice in a row must return nil the second time (idempotent — `os.RemoveAll` returns nil for missing paths).

### 7.3 `internal/cli/hook_test.go`

Tests must run the `runHook` function in-process. To redirect stdin/stderr, the test sets `os.Stdin` and `os.Stderr` temporarily (defer-restore) and uses `t.Setenv` for env vars.

#### Test `TestRunHook_HappyPath`
- Set up a temp DB via `db.Open` (applies schema), insert a sessions row and a session_summary row with all counters 0.
- `t.Setenv("AGENTRUN_SESSION_ID", sessionID)` and `t.Setenv("AGENTRUN_DB_PATH", dbPath)`.
- Pipe a fake PreToolUse payload to stdin: `{"session_id":"abc","cwd":"/tmp","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`.
- Use `os.Pipe()` to create a stdin replacement; defer-restore.
- Call `runHook([]string{"PreToolUse"})`.
- Assert `err == nil`.
- Assert: one new row in `events` for `sessionID` with `type='tool.pre_use'`, `source='hook'`, `payload_json` exactly equal to the input (as bytes after a JSON round-trip — we don't reformat).
- Assert `session_summary.tool_calls == 1`.

#### Test `TestRunHook_Orphan_NoSessionID`
- Do NOT set `AGENTRUN_SESSION_ID` (use `t.Setenv("AGENTRUN_SESSION_ID", "")`).
- Set `AGENTRUN_DB_PATH` to a valid DB.
- Pipe payload, call `runHook(["PreToolUse"])`.
- Assert `err == nil`.
- Assert `events` table count = 0 for the would-be session.

#### Test `TestRunHook_Orphan_NoDBPath`
- Set `AGENTRUN_SESSION_ID="s_x"`, unset `AGENTRUN_DB_PATH`.
- Call → assert nil, no panic.

#### Test `TestRunHook_BadJSON_StillExitsZeroWithWrapper`
- Stdin contains `not_json`.
- Setup proper env + sessions row.
- Call → assert `err == nil`.
- Assert one row inserted with `payload_json` matching `{"agentrun_invalid_payload":true,...}` shape (parse it via `json.Unmarshal` into a map and assert `agentrun_invalid_payload == true`).

#### Test `TestRunHook_TruncatesLargePayload`
- Build a stdin payload > 256 KiB (e.g. `{"x":"<repeated A 300000 times>"}`).
- Call. Assert one row with `payload_json` parseable as `{"agentrun_truncated": true, "original_size": > 256*1024, ...}`.

#### Test `TestRunHook_SessionStart_UpdatesModel`
- Stdin: `{"session_id":"...","cwd":"/","hook_event_name":"SessionStart","model":"claude-sonnet-4-7","transcript_path":"/tmp/t.jsonl"}`.
- Call `runHook(["SessionStart"])`.
- Assert `sessions.model == "claude-sonnet-4-7"` and `sessions.transcript_path == "/tmp/t.jsonl"`.

#### Test `TestRunHook_UnknownEventStoredVerbatim`
- Stdin: `{"hook_event_name":"SubagentStart","whatever":1}`.
- Call `runHook(["SubagentStart"])`.
- Assert one row with `type == "hook.SubagentStart"`.

### 7.4 `internal/db/queries_test.go` extensions (add to existing test file, no new file)

#### Test `TestInsertEventWithAutoSeq_AssignsConsecutiveSeq`
- Insert 5 events sequentially. Assert sequences are 1,2,3,4,5.

#### Test `TestInsertEventWithAutoSeq_ConcurrentNoCollisions`
- Spawn 50 goroutines, each inserts 4 events using `InsertEventWithAutoSeq` against the same session_id.
- Wait. Query all rows for that session, sort by sequence.
- Assert COUNT(*) == 200.
- Assert `MAX(sequence) >= 200` (gaps allowed under heavy contention; document this in the test comment).
- Assert NO `UNIQUE constraint failed` errors slipped through to the caller (collect errors via channel; assert empty).

#### Test `TestIncrementSummaryCounter_AllowlistRejectsBadInput`
- `IncrementSummaryCounter(db, sid, "exit_code")` returns a non-nil error mentioning `"allowlist"`.
- `IncrementSummaryCounter(db, sid, "tool_calls")` returns nil.

#### Test `TestIncrementSummaryCounter_AtomicUnderConcurrency`
- 100 goroutines each call `IncrementSummaryCounter(db, sid, "tool_calls")`.
- Final value of `tool_calls` must equal 100 exactly (atomic increments under WAL).

#### Test `TestUpdateSessionModel_RoundTrip` and `TestUpdateSessionTranscriptPath_NullGuard`
- Update model → re-read → assert equal.
- Update transcript_path on a row where it's NULL → succeeds. Update again with a different path → succeeds-but-no-op (verify the original path persists, because the WHERE clause guards on NULL).

### 7.5 Build constraints

All test files are pure Go and run on macOS + Linux. No new build tags. `hook_test.go` does NOT require a real `claude` binary.

---

## 8. Manual test checklist (extends Phase 1 §8.3)

Run these in order on macOS with a real `claude` binary on PATH. Each step yields a pass/fail. Tests 1–7 below assume `claude` is installed and authenticated.

1. **Build.** `CGO_ENABLED=0 go build -o /tmp/agentrun ./cmd/agentrun`. Assert no errors.
2. **Hook generation smoke.** Wrap and quit immediately:
   ```bash
   echo -e "/quit\n" | /tmp/agentrun claude
   ```
   Assert `<dbdir>/sessions/<session_id>/hooks.json` existed during the session (you can verify by adding a temporary `sleep 5` in claude, OR by tailing the dir from another terminal). After exit, the dir should be gone (cleanup ran).
3. **Real-session event smoke.** Run interactively:
   ```bash
   /tmp/agentrun claude
   ```
   In Claude, type a simple prompt like `"What is 2+2?"`, wait for the response, then quit.
4. **Inspect events.**
   ```bash
   /tmp/agentrun sessions     # confirm one session listed
   /tmp/agentrun show s_<id>  # confirm event-type breakdown
   ```
   Assert the breakdown shows at least: `hook.session_start (1)`, `user.prompt (>=1)`, `tool.pre_use (>=0)`, `response.stopped (>=1)`, `hook.session_end (1)`, plus the Phase 1 `session.started`, `session.ended`, `terminal.output`, `terminal.stdin` events.
5. **Verify model captured.**
   ```bash
   sqlite3 <dbdir>/agentrun.db "SELECT id, model FROM sessions ORDER BY started_at DESC LIMIT 1;"
   ```
   Assert `model` is non-empty (e.g. `claude-sonnet-4-7`).
6. **Verify counters.**
   ```bash
   sqlite3 <dbdir>/agentrun.db "SELECT user_prompts, tool_calls, approvals_request, errors FROM session_summary ORDER BY rowid DESC LIMIT 1;"
   ```
   Assert `user_prompts >= 1`. `tool_calls` may be 0 for a math question; if you asked Claude to run a Bash command, assert `>= 1`.
7. **Debug log clean.** Run with `--debug=hooks`:
   ```bash
   /tmp/agentrun claude --debug=hooks 2>/tmp/claude-hooks.log
   ```
   Run one prompt, quit. `grep -i "hook error" /tmp/claude-hooks.log` MUST return zero lines. (Any hook error means our hook command failed.)
8. **Transcript path stored.**
   ```bash
   sqlite3 <dbdir>/agentrun.db "SELECT transcript_path FROM sessions ORDER BY started_at DESC LIMIT 1;"
   ```
   Assert non-empty and the file exists on disk (`ls -la "$(...)"`).
9. **User settings still merge.** Add a no-op user hook to `~/.claude/settings.json`:
   ```json
   { "hooks": { "PreToolUse": [{ "matcher": "", "hooks": [{ "type": "command", "command": "true", "timeout": 5 }] }] } }
   ```
   Run a wrapped Claude session and confirm both the user's `true` hook AND our `agentrun hook PreToolUse` fired (use `--debug=hooks` log to see both). Remove the test entry after.
10. **`--bare` skips hooks (documented limitation).** `/tmp/agentrun claude --bare --help` (or any quick `--bare` invocation). After exit, assert ZERO `source='hook'` events for that session. PTY events MUST still be present.
11. **Orphan invocation safety.** From a separate terminal:
    ```bash
    echo '{}' | /tmp/agentrun hook PreToolUse
    ```
    Assert exit 0, stderr says `agentrun hook: orphan invocation...`, no rows added anywhere.
12. **Codex still works (regression).** `/tmp/agentrun codex --help` must run unchanged — no hooks.json file is created under Codex, no `--settings` injected.

---

## 9. Verification commands (run before declaring Phase 2 done)

```bash
# Build & smoke
CGO_ENABLED=0 go build -o /tmp/agentrun ./cmd/agentrun
/tmp/agentrun help                        # Should show the new "hook" line

# Unit + race tests
go test ./...
go test -race ./internal/db/...
go test -race ./internal/hooks/...
go test -race ./internal/cli/...

# Lint
go vet ./...
gofmt -d .                                # Must print nothing

# Static linkage (still pure Go)
otool -L /tmp/agentrun                    # macOS — no libsqlite
ldd /tmp/agentrun || true                 # Linux — "not a dynamic executable"

# DB inspection after a real session
sqlite3 .agentrun/agentrun.db <<'SQL'
SELECT type, source, COUNT(*) FROM events
WHERE session_id = (SELECT id FROM sessions ORDER BY started_at DESC LIMIT 1)
GROUP BY type, source ORDER BY type;

SELECT user_prompts, tool_calls, approvals_request, approvals_denied, errors
FROM session_summary
WHERE session_id = (SELECT id FROM sessions ORDER BY started_at DESC LIMIT 1);

SELECT model, transcript_path
FROM sessions ORDER BY started_at DESC LIMIT 1;
SQL
```

**Expected output shape (after a 1-prompt session that ran `Bash` once):**

```
hook.session_end|hook|1
hook.session_start|hook|1
notification|hook|0+
response.stopped|hook|1
session.ended|system|1
session.started|system|1
terminal.output|pty|N
terminal.stdin|pty|N
tool.post_use|hook|1
tool.pre_use|hook|1
user.prompt|hook|1
```

```
1|1|0|0|0
```

```
claude-sonnet-4-7|/Users/.../.claude/projects/.../<claude-sess-id>.jsonl
```

---

## 10. Risks & mitigations specific to Phase 2

### R1 — Hook subprocess startup latency exceeds 50ms target
- **Risk:** `agentrun hook` is invoked twice per tool call (PreToolUse + PostToolUse). On a session that runs 100 tools, that's 200 subprocess spawns. If each costs 100ms, that's 20s of wall-clock latency added to the session.
- **Mitigation:** `db.OpenReadWrite` skips migrations (the biggest single cost). The hook reads stdin, parses one small JSON, runs one INSERT (in a tx) + maybe one UPDATE. On a warm cache this should be 5–15ms on macOS. We measure during the manual checklist (step 4) by running `claude --debug=hooks 2>&1 | grep -i timing` and visually checking durations.
- **Escape valve:** if measurements show >50ms p50, document and consider a long-lived hook receiver (HTTP collector) in a future phase. Do NOT prematurely optimize in Phase 2.

### R2 — Cross-process sequence collisions under heavy concurrency
- **Risk:** Two hook processes fire at the same moment, both compute `MAX(sequence)+1 = 42`, both attempt to INSERT with sequence=42. The second hits the UNIQUE constraint and retries; with bad luck a third process collides on retry.
- **Mitigation:** Each retry is wrapped in a fresh BEGIN/COMMIT. The `SELECT MAX(sequence)+1` re-reads the post-conflict state. Three retries with jittered backoff (10/50/200ms) is sufficient for the firing rate we expect (Claude fires <50 hook events per second peak). The unit test `TestInsertEventWithAutoSeq_ConcurrentNoCollisions` validates this with 200 concurrent inserts.
- **Documented loss:** events that fail after 3 retries are logged to stderr (`agentrun hook: insert failed (event=... session=...): ...`) and dropped. Claude's transcript JSONL still has them.

### R3 — `--settings` arg ordering edge case
- **Risk:** If the user passes a conflicting `--settings` later in argv, Claude's behavior on duplicate flags is the **last one wins**. Our prepended `--settings <ours>` could be overridden.
- **Mitigation:** Document this in `agent_run.go` near the prepend logic. The realistic scenario is "user wants to test their own settings file" — in that case they explicitly opt out of our recording, which is fine. We do NOT add complexity to detect/refuse this.

### R4 — Settings file leak on wrapper crash
- **Risk:** If `runAgent` panics between `GenerateSettings` and the cleanup call, `<dbdir>/sessions/<sid>/hooks.json` is left behind. Over many crashed sessions this grows unbounded.
- **Mitigation:** Best-effort. The file is tiny (~2 KB). Document in a code comment. If it becomes a real problem, add a sweep on startup: on each `runAgent` invocation, scan `<dbdir>/sessions/`, delete dirs whose session_id row in `sessions` has a non-NULL `ended_at`. Deferred until measured impact.

### R5 — Path with shell metacharacters
- **Risk:** If the user's home contains a space, semicolon, or backtick (rare but possible), the hook command string injected into hooks.json could be mis-parsed by Claude when it executes via `sh -c "<command>"`.
- **Mitigation:** `shellQuote` in `hooks.go` covers spaces and common metacharacters via single-quote wrapping. The full path includes `agentrun hook <EventName>` where `<EventName>` is from our hard-coded table — no user input ever enters the command string.

---

## 11. Implementation order

Build in this order to keep `go build` and `go test ./...` green at every step.

1. **`internal/hooks/normalize.go` + `normalize_test.go`** — pure functions, no deps. `go test ./internal/hooks/...` must pass.
2. **`internal/hooks/hooks.go` + `hooks_test.go`** — pure file I/O. `go test ./internal/hooks/...` must pass.
3. **`internal/db/db.go`** — add `OpenReadWrite`. Run existing db tests; they must still pass.
4. **`internal/db/queries.go`** — add `InsertEventWithAutoSeq`, `IncrementSummaryCounter`, `UpdateSessionModel`, `UpdateSessionTranscriptPath`, and helpers. Extend `queries_test.go` (or `db_test.go`) with the new tests. `go test -race ./internal/db/...` must pass.
5. **`internal/cli/hook.go` + `hook_test.go`** — the subcommand handler. Tests use in-process I/O redirection. `go test ./internal/cli/...` must pass.
6. **`internal/cli/root.go`** — add the `case "hook":` arm and update usage text.
7. **`internal/cli/agent_run.go`** — three insertions: `resolveSelfPath`, `GenerateSettings` call, `--settings`+env injection, `hooks.Cleanup` call. Build must succeed.
8. **Manual test pass.** Run §8 checklist on macOS with real `claude`. Fix anything that doesn't behave.
9. **Final verification.** Run §9 commands.

---

## 12. Out-of-scope reminder for Phase 2

If during implementation the implementer feels tempted to add any of the following, **stop and defer**:

- ANY Codex-specific hook code. Codex is a separate phase.
- HTTP hook receivers.
- Hooks for events outside the MVP table (no SubagentStart, no FileChanged, etc.).
- A persistent process that batches hook events (defer until R1 measured).
- A lookup table mapping Claude `session_id` ↔ our `s_<ulid>`.
- Copying transcript JSONL into our artifacts.
- Any new schema columns or tables.
- Real regex redaction (still Phase 5+).
- A `transcript_path` extraction from PostToolUse / Stop (we capture it from SessionStart and that's sufficient).
- Hook output JSON to influence Claude (we are read-only).

Phase 2 is done when:
- All §7 tests pass under `go test -race ./...`.
- All §8 manual checklist items pass on macOS with the real `claude` CLI.
- §9 verification commands print the expected output.
- `go vet ./...` and `gofmt -d .` produce no diff.
