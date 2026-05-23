# agentrun

Read-only ingester for Claude Code and Codex session transcripts. Watches the
JSONL files those agents already write to disk, normalizes them, and stores
them in Postgres for later analysis and skill synthesis.

No hooks. No wrapper. No PTY. No install on every laptop. The agents write
the data; we just read it.

## Why

Claude and Codex each write a complete transcript of every session to a
well-known directory on disk:

- Claude → `~/.claude/projects/<derived>/<session-uuid>.jsonl`
- Codex  → `~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl`

That's everything: prompts, model replies, every tool call with full input and
output. agentrun tails those files, parses both formats into the same row
shape, and writes to Postgres. From there you query, analyze, and build skills.

## Install

Requires Go 1.22+ and a running Postgres (with `pgvector` extension —
managed providers and the `pgvector/pgvector` Docker image both ship it).

```bash
# Local Postgres via Docker:
docker compose up -d

# Connection string:
export DATABASE_URL='postgres://agentrun:agentrun@localhost:5433/agentrun'

# Build + run:
go run ./cmd/agentrun watch
```

Apply the schema on first run (the binary does this automatically; or run
`psql $DATABASE_URL -f schema.sql` yourself).

## Commands

```
agentrun watch                 # tail JSONL forever, ingest as files grow
agentrun sync                  # one-shot catch-up; exits when done
agentrun list                  # print 30 most recent sessions
agentrun show <session_uuid>   # print every event in one session
```

`watch` first does a full sweep to backfill anything written while it was
down, then sits on `fsnotify` events. Ingestion is per-transaction with the
file's byte-offset, so a kill -9 at any moment leaves no duplicates and no
gaps.

## Multi-user

Set `AGENTRUN_USER=alice@example.com` (or any string) before launching the
watcher. Every session row gets stamped with that name. Pointing multiple
laptops at one shared Postgres gives you a team-wide audit log.

## Schema

```sql
sessions       (agent, session_uuid) primary key, user_name, cwd, model, …
events         every JSONL line: seq, ts, role, tool_name, content, payload
ingest_state   per-file (inode, byte_offset) checkpoint
```

`role` is normalized to: `user | assistant | tool_use | tool_result | system`.
`payload` keeps the original JSONL line verbatim for forensic queries.

## What's next

This is the ingestion half. The skill synthesis half — clustering sessions
by embedding, mining recurring patterns, generating `~/.claude/skills/<name>/`
files — is the next phase, built on top of this same DB.
