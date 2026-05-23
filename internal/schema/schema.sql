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
