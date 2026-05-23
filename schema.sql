-- agentrun schema. Single file, plain Postgres + pgvector. Apply with:
--   psql $DATABASE_URL -f schema.sql
-- Idempotent — every object uses IF NOT EXISTS.

CREATE EXTENSION IF NOT EXISTS vector;

-- One row per agent session. (agent, session_uuid) is the natural key —
-- we use the agent's own UUID directly, no synthesised IDs.
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

-- One row per JSONL line we ingest. `seq` is the line number in the source
-- file (1-based) — gives us deterministic ordering without trusting agents'
-- timestamps. payload holds the original line verbatim for forensic queries.
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

CREATE INDEX IF NOT EXISTS events_session
    ON events(agent, session_uuid, seq);
CREATE INDEX IF NOT EXISTS events_role     ON events(role);
CREATE INDEX IF NOT EXISTS events_ts       ON events(ts);
CREATE INDEX IF NOT EXISTS events_tool     ON events(tool_name) WHERE tool_name IS NOT NULL;
-- GIN index over the JSON payload so ad-hoc queries are fast.
CREATE INDEX IF NOT EXISTS events_payload_gin
    ON events USING gin(payload jsonb_path_ops);

-- Tracks how far we've read into each JSONL file. (inode, byte_offset)
-- detects rotation/replacement. Updated atomically with each event insert
-- so a crash mid-line leaves things consistent.
CREATE TABLE IF NOT EXISTS ingest_state (
    file_path   TEXT        PRIMARY KEY,
    inode       BIGINT      NOT NULL,
    byte_offset BIGINT      NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
