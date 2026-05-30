# agentrun

Read-only ingester for Claude Code and Codex session transcripts. Watches the
JSONL files those agents already write to disk, normalises them, and stores
them in Postgres for later analysis and skill synthesis.

No hooks. No wrapper. No PTY. The agents write the data; we just read it.

## Install (one-liner)

Requires Docker (for Postgres). macOS and Linux, arm64 or amd64.

```bash
curl -fsSL https://raw.githubusercontent.com/AI4Everyonee/agentrun/main/install.sh | bash
```

That command:

1. Starts a `pgvector/pgvector:pg16` container with `--restart unless-stopped`
   on port 5433, backed by a named volume so data survives reboots.
2. Downloads the right binary for your OS/arch into `~/.local/bin/agentrun`.
3. Writes a config at `~/.config/agentrun/config.env`.
4. Installs a background service (launchd on macOS, systemd-user on Linux)
   that runs `agentrun watch` at login, restarts on crash, and survives logout.
5. Runs `agentrun status` to confirm everything is healthy.

After install, every Claude Code or Codex session on this machine flows into
Postgres automatically — no further action needed.

## Why

Claude and Codex each write a complete transcript of every session to a
well-known directory on disk:

- Claude → `~/.claude/projects/<derived>/<session-uuid>.jsonl`
- Codex  → `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`

That's everything: prompts, model replies, every tool call with full input and
output. agentrun tails those files, parses both formats into the same row
shape, and writes to Postgres. From there you query, analyse, and build skills.

## Commands

```
agentrun watch                       # tail JSONL forever, ingest as files grow
agentrun sync [--since 14d]          # one-shot catch-up; exits when done
agentrun list                        # print 30 most recent sessions
agentrun show <session_uuid>         # print every event in one session
agentrun status                      # DB / container / watcher health + counts
agentrun paths                       # print Claude and Codex transcript roots
```

`watch` first does a full sweep so anything written while it was down is
caught up, then sits on `fsnotify` events.

## Status check

```bash
agentrun status
```

Reports whether the database is reachable, whether the Postgres container is
running, whether a watcher process is alive, and how many sessions / events
are stored. Exit code is non-zero if anything is unhealthy — useful in cron
or monitoring.

## Config

Settings are read from environment variables or `~/.config/agentrun/config.env`
(XDG: `$XDG_CONFIG_HOME/agentrun/config.env`). Environment variables take
precedence over the file.

| Variable              | Default                  | Description                                          |
|-----------------------|--------------------------|------------------------------------------------------|
| `DATABASE_URL`        | *(required)*             | Postgres DSN — needed by watch, sync, list, show, status |
| `AGENTRUN_USER`       | `$USER`                  | Identity stamped on every ingested session           |
| `AGENTRUN_CLAUDE_ROOT`| `~/.claude/projects`     | Override the Claude transcript root directory        |
| `AGENTRUN_CODEX_ROOT` | `~/.codex/sessions`      | Override the Codex transcript root directory         |

`agentrun paths` always prints the resolved roots (respecting any overrides)
without needing `DATABASE_URL`.

## Multi-user

Edit `~/.config/agentrun/config.env` and set `AGENTRUN_USER=alice@example.com`,
then restart the service:

```bash
# macOS
launchctl kickstart -k gui/$UID/com.agentrun.watcher

# Linux
systemctl --user restart agentrun
```

Every session row gets stamped with that name. Point multiple laptops at one
shared Postgres (set `DATABASE_URL` accordingly in `config.env`) and you get a
team-wide audit log.

## Uninstall

```bash
./uninstall.sh           # remove service + binary; keep Postgres + data
./uninstall.sh --purge   # also remove the container and its volume (deletes data)
```

## Crash-safe ingestion

Three independent guards against duplicate or lost data:

- `sessions` table: `PRIMARY KEY (agent, session_uuid)` — same session can be
  re-ingested forever without creating duplicates.
- `events` table: `UNIQUE (agent, session_uuid, seq)` with `ON CONFLICT DO
  NOTHING` — replaying the same JSONL line is a no-op.
- `ingest_state` table: per-file `(inode, byte_offset)` checkpoint, written in
  the **same transaction** as the events it covers. A `kill -9` mid-write
  leaves no gaps and no duplicates.

You can run `sync` and `watch` concurrently, or restart either at any moment,
and the database stays consistent.

## Schema

Three tables, defined as a single `const schemaSQL` block in `main.go`:

```
sessions       (agent, session_uuid) primary key + user_name, model, started_at, …
events         every JSONL line: seq, ts, role, tool_name, content, payload (jsonb)
ingest_state   per-file (inode, byte_offset) checkpoint
```

`role` is normalised to: `user | assistant | tool_use | tool_result | system`.
`payload` keeps the original JSONL line verbatim for forensic queries.
`pgvector` is enabled so embedding columns can land on `sessions` later
without a schema change.

## What's next

This is the ingestion half. The skill synthesis half — clustering sessions
by embedding, mining recurring patterns, generating `~/.claude/skills/<name>/`
files — is the next phase, built on top of this same DB.

## Development

Build from source (requires Go 1.22+):

```bash
git clone https://github.com/AI4Everyonee/agentrun
cd agentrun
go build -o agentrun .
```

To install from a clone using your local build instead of a release download:

```bash
./install.sh --local-build
```

To target a specific release version:

```bash
./install.sh --version v0.1.0
```

Manual Postgres (if you don't want install.sh to manage it):

```bash
docker run -d --name agentrun-postgres \
  --restart unless-stopped \
  -p 5433:5432 \
  -e POSTGRES_USER=agentrun \
  -e POSTGRES_PASSWORD=agentrun \
  -e POSTGRES_DB=agentrun \
  -v agentrun_pgdata:/var/lib/postgresql/data \
  pgvector/pgvector:pg16
```

Cutting a release: tag and push.

```bash
git tag v0.1.0
git push origin v0.1.0
```

The `.github/workflows/release.yml` workflow builds binaries for
darwin/arm64, darwin/amd64, linux/arm64, linux/amd64 and attaches them to
the GitHub release.
