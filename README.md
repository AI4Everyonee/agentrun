# agentrun

Local observability for Claude Code and Codex sessions. Every prompt, tool call, file change, validation run, and approval is recorded into a single SQLite database under `~/.agentrun/agentrun.db` — for **every** invocation on your laptop, not just sessions launched through a wrapper.

```bash
# One-time setup
agentrun install

# Just use claude / codex normally; everything is recorded.
claude
codex exec "..."

# Inspect
agentrun sessions                                  # list (with USER column on shared DBs)
agentrun show <session_id>                         # single-session detail
agentrun export <session_id> --output session.jsonl
agentrun search "rate limit"                       # FTS5 across all event payloads
agentrun stats                                     # per-agent / per-repo / per-user rollup
agentrun diff <session_id>                         # pipe captured git.diff through $PAGER
agentrun replay <session_id>                       # asciinema-style PTY playback (wrapped only)
agentrun compare s_a s_b                           # side-by-side event timeline
agentrun watch --filter tool.pre_use               # tail events live
agentrun tag <session_id> production-deploy        # annotate

# Maintenance
agentrun doctor                                    # installation health check
agentrun finalize-idle --older-than 30m            # sweep stale 'running' sessions
agentrun gc --older-than 30d                       # delete old sessions + artifacts

# Optional: long-lived collector for sessions with hundreds of tool calls
agentrun collector start                           # ~10ms→~1ms per hook
agentrun collector status
agentrun collector stop
```

**Multi-user / cloud mode.** Set `AGENTRUN_USER=alice@example.com` in your
shell rc; every session row gets stamped with that identity. Combined with a
shared DB path (e.g. a Litestream-replicated SQLite or a network-mounted file),
this gives you a team-wide audit trail.

---

## What it captures

| Source | Events | Origin |
|---|---|---|
| Hooks | `user.prompt`, `tool.pre_use`, `tool.post_use`, `tool.failed`, `approval.requested`, `approval.responded`, `notification`, `response.stopped`, `hook.session_start`, `hook.session_end`, `context.pre_compact` | Claude Code / Codex lifecycle hooks |
| PTY | `terminal.output`, `terminal.stdin`, `session.started`, `session.ended` | Wrapper only (`agentrun claude` / `agentrun codex`) |
| Filesystem | `file.modified`, `file.deleted` | Wrapper only (fsnotify on the session cwd) |
| Validation | `validation.completed` | Hook command pattern-matches Bash commands |
| System | `git_diff` artifact, `start_commit_sha`, `end_commit_sha`, branch | Both flows |

Every event carries `session_id`, `sequence`, `ts`, `source`, `type`, and the raw hook payload as JSON. Secrets matching common patterns (Anthropic / OpenAI / GitHub / AWS keys, JWTs, bearer tokens, `.env` assignments, PEM blocks) are redacted before persistence.

---

## Install

**One-liner** (macOS or Linux, amd64 or arm64):

```bash
curl -fsSL https://raw.githubusercontent.com/AI4Everyonee/agentrun/main/install.sh | bash
```

This downloads the latest binary from GitHub releases, drops it in
`/usr/local/bin` (or `~/.local/bin` if not writable), and runs
`agentrun install` to register the global Claude + Codex hooks. No auth
required — the repo is public.

**Other options:**

```bash
# Pin a specific version
AGENTRUN_VERSION=v0.4.0 curl -fsSL .../install.sh | bash

# Install the binary only, skip hook setup
AGENTRUN_SKIP_HOOKS=1 curl -fsSL .../install.sh | bash

# Build from source (requires Go 1.22+)
go install github.com/AI4Everyonee/agentrun/cmd/agentrun@latest
agentrun install

# Or clone:
git clone https://github.com/AI4Everyonee/agentrun
cd agentrun
CGO_ENABLED=0 go build -o /usr/local/bin/agentrun ./cmd/agentrun
agentrun install
```

**Optional: OpenAI session summaries.** Set `OPENAI_API_KEY` in your shell
rc; every session gets a one-paragraph summary at end-of-session by
calling `gpt-5.4-mini` (override with `AGENTRUN_SUMMARY_MODEL`). Without
the key, the summarizer is a no-op — everything else still records.

This:
- Merges hook entries into `~/.claude/settings.json` (your existing settings preserved; original backed up to `.bak.<ts>`).
- Appends a marker block to `~/.codex/config.toml` (between `# === AGENTRUN HOOKS BEGIN/END ===`).
- Creates `~/.agentrun/agentrun.db` on first session.

**Codex one-time trust review.** The first time you run Codex interactively after installing, it'll prompt:

> 7 hooks need review before they can run. Open /hooks to review them.

Run `codex` (no args), type `/hooks`, press Enter, approve each entry. After that, every Codex session (interactive and `codex exec`) is recorded. Until trust is granted, Codex sessions record only via the wrapper's PTY capture.

To remove:

```bash
agentrun uninstall
```

User-authored hooks are preserved; only entries with `agentrun hook claude …` / `agentrun hook codex …` are stripped.

---

## Usage

### Inspect sessions

```bash
agentrun sessions
```

```
SESSION ID                      AGENT   REPO                          STARTED              STATUS
s_native_codex_019e5398...      codex   /Users/me/proj                2026-05-23 12:19:55  running
s_01KS9TKMR5...                 claude  /Users/me/proj                2026-05-23 12:08:19  completed
```

Native sessions (no wrapper) get IDs like `s_native_<agent>_<their_uuid>`. Wrapper sessions get ULIDs prefixed `s_01`.

### Drill into one session

```bash
agentrun show s_01KS9TKMR5WHC5KSDXJW6NKZR7
```

```
Session: s_01KS9TKMR5WHC5KSDXJW6NKZR7
Agent: claude (2.1.149 (Claude Code))
Repo: /Users/me/proj
Branch: main
Start commit: dc001c0d
End commit:   dc001c0d
Started: 2026-05-23T07:08:19Z
Ended:   2026-05-23T07:08:28Z
Exit code: 0

Events:
  file.deleted          : 2
  file.modified         : 1
  hook.session_end      : 1
  hook.session_start    : 1
  response.stopped      : 1
  session.ended         : 1
  session.started       : 1
  terminal.output       : 2
  tool.post_use         : 2
  tool.pre_use          : 2
  user.prompt           : 1
  validation.completed  : 1

Artifacts:
  terminal_log  pty.raw    93 B
  git_diff      git.diff   3.3 KB
```

For Codex sessions, a `Turns:` block appears below `Events:` grouping per-`turn_id`.

### Export

```bash
agentrun export <id>                            # JSONL to stdout
agentrun export <id> --output session.jsonl     # to file
```

The first line is a header (`{"agentrun_export_version":"1","session":{…},"artifacts":[…]}`). Each subsequent line is one event with `payload` parsed as a nested object (not a string), so you can drill in with `jq`:

```bash
jq -c 'select(.type=="tool.pre_use") | .payload.tool_input.command' session.jsonl
```

### Sweep stale sessions

Codex has no `SessionEnd` hook event, so its native sessions stay `status=running` forever. Run periodically (or wire it into a cron):

```bash
agentrun finalize-idle                   # default: 30m
agentrun finalize-idle --older-than 5m   # tighter
```

### Wrapper mode (opt-in)

For maximum capture, prefix your invocation:

```bash
agentrun claude
agentrun codex exec "..."
```

This adds, on top of the global hook events:
- Full PTY transcript on disk (`<sid>/pty.raw`)
- Filesystem watcher events for every file change in cwd
- Reliable `exit_code` capture

The wrapper sets `AGENTRUN_SESSION_ID` and `AGENTRUN_DB_PATH`, which the hook command honors instead of synthesizing a native session ID, so the wrapper and hooks share one session row.

---

## Architecture

```
                          ┌─────────────────────┐
        claude / codex ───▶  hook subprocess     │ ◀── fired by ~/.claude/settings.json
                          │  agentrun hook …     │     or ~/.codex/config.toml
                          └──────────┬──────────┘
                                     │ INSERT
                                     ▼
                          ┌─────────────────────┐
                          │  ~/.agentrun/       │
                          │  agentrun.db        │ ◀── recorder (wrapper) also writes here
                          │  + artifacts/       │
                          └─────────────────────┘
                                     ▲
        agentrun claude / codex ─────┘ (PTY + fswatcher emit into same session row)
```

- **Single SQLite DB** at `~/.agentrun/agentrun.db`. WAL mode with `_txlock=immediate` for cross-process write concurrency.
- **Hook command** (`agentrun hook <agent> <event>`) is a one-shot subprocess Claude/Codex fires. ~10ms warm-cache cost.
- **Wrapper** (`agentrun claude` / `agentrun codex`) is a long-lived PTY+fsnotify host that adds PTY capture and a filesystem watcher.
- **Cross-process event sequencing**: every writer uses `MAX(sequence)+1` inside a transaction with retry on `SQLITE_BUSY` / `UNIQUE` collisions.

---

## Schema

```sql
CREATE TABLE sessions (
  id, agent, agent_version, model, permission_mode,
  cwd, repo_root, branch,
  start_commit_sha, end_commit_sha,
  started_at, ended_at, exit_code,
  transcript_path, pid, metadata_json
);

CREATE TABLE events (
  id, session_id, sequence, ts,
  source,                   -- 'hook' | 'pty' | 'filesystem' | 'validation' | 'system'
  type,                     -- 'tool.pre_use' | 'user.prompt' | ...
  payload_json,             -- raw hook payload (redacted)
  redaction_version,        -- 'regex-1' or 'noop-1'
  created_at,
  UNIQUE(session_id, sequence)
);

CREATE TABLE artifacts (
  id, session_id, kind,     -- 'terminal_log' | 'git_diff'
  path, content_hash, size_bytes, mime, metadata_json
);

CREATE TABLE session_summary (
  session_id,
  user_prompts, tool_calls, files_changed, commands_run,
  validations_run, validations_pass, validations_fail,
  approvals_request, approvals_denied, errors,
  status              -- 'running' | 'completed' | 'failed'
);
```

---

## Known limitations

- **Codex sessions don't auto-finalize** — Codex doesn't emit `SessionEnd`. Use `agentrun finalize-idle` to sweep them up.
- **Native sessions don't get a PTY transcript or fswatcher events** — those require the wrapper. The hook payloads still capture every tool call.
- **`claude --bare` skips hooks** by design (Claude documents this). Wrapped sessions still get PTY capture in that mode.
- **First-time Codex sessions don't record** — until you do the `/hooks` trust review.
- **`model` column may be NULL for Claude sessions** — Claude's hook payloads don't actually include a `model` field despite what some docs suggest. Codex does include it on `PreToolUse`.

---

## Querying the DB directly

The DB is plain SQLite. Open it with any client:

```bash
sqlite3 ~/.agentrun/agentrun.db

-- Sessions by tool usage
SELECT s.id, s.agent, ss.tool_calls, ss.errors
FROM sessions s JOIN session_summary ss ON ss.session_id = s.id
ORDER BY ss.tool_calls DESC LIMIT 10;

-- Every Bash command Claude has run, with its raw command line
SELECT json_extract(payload_json, '$.tool_input.command')
FROM events
WHERE type = 'tool.pre_use'
  AND json_extract(payload_json, '$.tool_name') = 'Bash'
ORDER BY ts DESC LIMIT 50;
```

---

## License

MIT (or whatever — see `LICENSE`).
