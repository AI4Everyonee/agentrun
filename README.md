# agentrun

Read-only ingester for Claude Code and Codex session transcripts. Watches the
JSONL files those agents already write to disk, normalises them, and stores
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
shape, and writes to Postgres. From there you query, analyse, and build skills.

## Install

Requires Go 1.22+ and a running Postgres (with the `pgvector` extension).

```bash
# Local Postgres + pgvector via Docker (one shot):
docker run -d --name agentrun-postgres \
  -p 5433:5432 \
  -e POSTGRES_USER=agentrun \
  -e POSTGRES_PASSWORD=agentrun \
  -e POSTGRES_DB=agentrun \
  pgvector/pgvector:pg16

# Build + run:
export DATABASE_URL='postgres://agentrun:agentrun@localhost:5433/agentrun'
go run . sync                        # backfill the last 14 days
go run . watch                       # tail forever
```

The schema is embedded in the binary and applied automatically on first
connect — nothing to provision.

## Commands

```
agentrun watch                       # tail JSONL forever, ingest as files grow
agentrun sync [--since 14d]          # one-shot catch-up; exits when done
agentrun list                        # print 30 most recent sessions
agentrun show <session_uuid>         # print every event in one session
```

`watch` first does a full sweep so anything written while it was down is
caught up, then sits on `fsnotify` events. Ingestion is per-line in one
transaction with the file's byte-offset checkpoint, so a kill -9 at any
moment leaves no duplicates and no gaps.

## Multi-user

Set `AGENTRUN_USER=alice@example.com` (or any string) before launching the
watcher. Every session row gets stamped with that name. Point multiple
laptops at one shared Postgres and you get a team-wide audit log.

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
