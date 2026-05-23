# Goal: Local Agent Session Recorder

## 1. Objective

Build a local observability system that records every client-side action performed during Claude Code and Codex sessions.

The system must capture:

- What the user asked
- What the agent executed locally
- What files changed
- What tool calls were made
- What outputs were produced
- What approvals were requested
- What failed
- What passed
- What evidence proves the session was completed correctly

This system does **not** attempt to capture hidden model reasoning or server-side execution inside Anthropic/OpenAI infrastructure.

---

## 2. Problem Statement

Coding agents such as Claude Code and Codex perform software development work through terminal commands, file edits, tool calls, prompts, approvals, local context gathering, and validation steps.

Today, these actions are difficult to audit, replay, compare, debug, or enforce consistently.

A user can see some terminal output, but there is no durable, normalized, queryable session record showing the full client-side execution path.

We need a local session recorder that creates an append-only event log for every agent run.

---

## 3. Core Goal

Create a local execution harness that launches coding agents and records all observable client-side activity into a database.

Users must run agents through a wrapper command:

```bash
agentrun claude
agentrun codex
```

The wrapper should start a new session, attach terminal capture, enable native hooks, observe filesystem changes, track Git state, and persist all events.

---

## 4. Non-Goals

This project will not:

- Capture hidden chain-of-thought
- Capture vendor-side system prompts
- Capture server-side execution inside Anthropic/OpenAI
- Depend on unstable transcript formats as the only source of truth
- Replace Claude Code or Codex
- Modify agent behavior in the MVP
- Perform network MITM interception in the MVP

Network capture may be considered later, but the MVP should focus on reliable local observability.

---

## 5. High-Level Architecture

```text
User
  |
  v
agentrun wrapper
  |
  v
PTY recorder
  |
  v
Claude Code / Codex process
  |
  +--> Native agent hooks
  |
  +--> Terminal stream capture
  |
  +--> Filesystem observer
  |
  +--> Git diff/status snapshots
  |
  v
Local collector API
  |
  v
Append-only database
  |
  v
Session inspection CLI / UI / JSONL export
```

---

## 6. Capture Layers

### 6.1 Terminal Capture Layer

Capture raw terminal activity for auditability.

Must capture:

- User-entered prompts
- stdin
- stdout
- stderr
- exit code
- terminal dimensions if needed
- command start/end timestamps
- process PID
- process environment metadata, excluding secrets

This layer acts as a fallback when structured hooks are incomplete.

---

### 6.2 Native Hook Layer

Use native Claude Code and Codex hooks wherever available.

Capture hook events such as:

- session start
- user prompt submit
- pre-tool-use
- post-tool-use
- tool failure
- permission request
- file change
- stop event
- session end
- transcript path
- current working directory
- model name, if exposed
- permission mode, if exposed

Each hook event should be persisted as raw JSON, plus normalized fields.

---

### 6.3 Filesystem Capture Layer

Observe file changes during the session.

Capture:

- file created
- file modified
- file deleted
- file renamed
- file path
- timestamp
- content hash
- file size
- optional before/after snapshots for text files

Use this to correlate tool calls with actual codebase changes.

---

### 6.4 Git Capture Layer

Git is the primary source of truth for code changes.

At minimum, capture:

- repo root
- current branch
- current commit SHA
- dirty status at session start
- dirty status at session end
- changed files
- staged files
- untracked files
- `git diff`
- `git diff --stat`
- `git status --porcelain`

Capture Git snapshots:

- at session start
- after meaningful file/tool events
- before validation
- after validation
- at session end

---

### 6.5 Validation Capture Layer

Capture verification evidence.

Examples:

- test commands
- build commands
- lint commands
- typecheck commands
- exit codes
- stdout/stderr
- duration
- pass/fail status
- generated reports
- coverage files
- artifacts

This layer is required for later enforcement of Definition of Done.

---

### 6.6 Approval and Permission Capture Layer

Capture moments where the agent asks for permission.

Must capture:

- approval request type
- requested command/tool
- risk category, if available
- user response
- timestamp
- whether the action proceeded
- whether the action was denied

---

## 7. Data Model

Use an append-only event model.

### 7.1 Sessions Table

```sql
CREATE TABLE sessions (
  id TEXT PRIMARY KEY,
  agent TEXT NOT NULL,
  agent_version TEXT,
  model TEXT,
  repo_root TEXT,
  cwd TEXT NOT NULL,
  branch TEXT,
  start_commit_sha TEXT,
  end_commit_sha TEXT,
  started_at TIMESTAMPTZ NOT NULL,
  ended_at TIMESTAMPTZ,
  exit_code INTEGER,
  transcript_path TEXT,
  metadata_json JSONB DEFAULT '{}'::jsonb
);
```

### 7.2 Events Table

```sql
CREATE TABLE events (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  ts TIMESTAMPTZ NOT NULL,
  source TEXT NOT NULL,
  type TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  payload_json JSONB NOT NULL,
  redaction_version TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Recommended `source` values:

- `pty`
- `hook`
- `filesystem`
- `git`
- `validation`
- `approval`
- `system`

Recommended `type` values:

- `session.started`
- `session.ended`
- `user.prompt`
- `terminal.stdin`
- `terminal.stdout`
- `terminal.stderr`
- `tool.pre_use`
- `tool.post_use`
- `tool.failed`
- `file.created`
- `file.modified`
- `file.deleted`
- `git.snapshot`
- `validation.started`
- `validation.completed`
- `approval.requested`
- `approval.responded`
- `error`

### 7.3 Artifacts Table

```sql
CREATE TABLE artifacts (
  id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL REFERENCES sessions(id),
  event_id TEXT REFERENCES events(id),
  kind TEXT NOT NULL,
  path TEXT,
  content_hash TEXT,
  size_bytes BIGINT,
  metadata_json JSONB DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
```

Recommended `kind` values:

- `git_diff`
- `terminal_log`
- `test_report`
- `coverage_report`
- `build_log`
- `transcript`
- `file_snapshot`
- `jsonl_export`

---

## 8. MVP Requirements

The MVP must support:

- Launching Claude Code through `agentrun claude`
- Launching Codex through `agentrun codex`
- Creating one session record per run
- Capturing terminal stream
- Capturing native hook JSON
- Capturing Git metadata
- Capturing Git diff snapshots
- Capturing changed files
- Capturing command outputs and exit codes where observable
- Capturing approval events where observable
- Redacting secrets before persistence
- Exporting a session as JSONL
- Inspecting a session from CLI

---

## 9. MVP Definition of Done

The MVP is complete only when all of the following are true:

### 9.1 Session Capture

- A new session ID is generated for every `agentrun` invocation.
- Session start and end are persisted.
- Agent name is persisted.
- Working directory is persisted.
- Repo root is detected when inside a Git repo.
- Start and end commit SHAs are persisted when available.

### 9.2 Terminal Capture

- stdout is captured.
- stderr is captured.
- stdin/user prompt input is captured where possible.
- Process exit code is captured.
- Terminal capture is associated with the correct session ID.

### 9.3 Hook Capture

- Claude Code hook events are captured when running Claude.
- Codex hook events are captured when running Codex.
- Raw hook JSON is persisted.
- Hook event order is preserved.

### 9.4 Git Capture

- Initial Git status is captured.
- Final Git status is captured.
- Final Git diff is captured.
- Changed file list is captured.
- Git capture does not fail the whole session when outside a Git repo.

### 9.5 Secret Redaction

- API keys are redacted.
- Known token patterns are redacted.
- `.env` file contents are not blindly persisted.
- Redaction is applied before writing to durable storage.
- Raw unredacted logs are never written by default.

### 9.6 Export

- Session can be exported as JSONL.
- Export preserves event order.
- Export includes session metadata.
- Export includes references to artifacts.

### 9.7 Inspection

A user can run:

```bash
agentrun sessions
agentrun show <session_id>
agentrun export <session_id> --format jsonl
```

---

## 10. Example CLI UX

### Start Claude

```bash
agentrun claude
```

### Start Codex

```bash
agentrun codex
```

### List Sessions

```bash
agentrun sessions
```

Example output:

```text
SESSION ID     AGENT    REPO              STARTED                  STATUS
s_01hxyz       claude   /repo/app         2026-05-23 10:41:10      completed
s_01habc       codex    /repo/api         2026-05-23 11:03:22      failed
```

### Show Session Summary

```bash
agentrun show s_01hxyz
```

Example output:

```text
Session: s_01hxyz
Agent: claude
Repo: /repo/app
Branch: main
Start Commit: abc123
End Commit: def456
Status: completed

Events:
- user prompts: 3
- tool calls: 18
- files changed: 7
- commands executed: 12
- validation commands: 3

Validation:
- npm test: passed
- npm run lint: passed
- npm run typecheck: passed
```

### Export Session

```bash
agentrun export s_01hxyz --format jsonl --output session.jsonl
```

---

## 11. Event JSON Shape

Every event should follow a common envelope.

```json
{
  "id": "evt_01hxyz",
  "session_id": "s_01hxyz",
  "ts": "2026-05-23T10:41:10.123Z",
  "sequence": 42,
  "source": "hook",
  "type": "tool.post_use",
  "payload": {
    "tool_name": "Bash",
    "tool_input": {
      "command": "npm test"
    },
    "tool_response": {
      "exit_code": 0,
      "stdout": "...",
      "stderr": ""
    }
  }
}
```

---

## 12. Secret Redaction Requirements

The recorder must redact likely secrets before persistence.

Redact:

- OpenAI API keys
- Anthropic API keys
- GitHub tokens
- AWS access keys
- JWTs
- private keys
- database URLs
- `.env` values
- bearer tokens
- OAuth tokens
- session cookies

The redactor should preserve structure while replacing values.

Example:

```text
ANTHROPIC_API_KEY=sk-ant-abc123
```

Becomes:

```text
ANTHROPIC_API_KEY=[REDACTED:ANTHROPIC_API_KEY]
```

---

## 13. Storage Choice

### MVP

Use SQLite for local-first development.

Benefits:

- simple
- portable
- no infrastructure required
- easy local inspection
- good enough for single-user local sessions

### Later

Add Postgres or ClickHouse if needed.

Use Postgres for:

- multi-user deployments
- team-wide audit logs
- relational querying
- durable centralized storage

Use ClickHouse for:

- high-volume logs
- analytics
- aggregate event queries
- long-term telemetry

---

## 14. Implementation Phases

### Phase 1: Local Session Wrapper

Build:

- `agentrun` CLI
- process launcher
- session ID generation
- environment injection
- SQLite session creation
- stdout/stderr capture
- exit code capture

Done when:

- `agentrun claude` and `agentrun codex` launch the real agents
- session start/end are stored
- terminal output is stored

---

### Phase 2: Hook Collector

Build:

- local HTTP collector or executable hook receiver
- Claude hook config generator
- Codex hook config generator
- raw hook JSON persistence
- event normalization

Done when:

- hook events appear in DB for Claude sessions
- hook events appear in DB for Codex sessions
- hook events are attached to the right session

---

### Phase 3: Git Snapshots

Build:

- repo root detector
- branch detector
- commit SHA detector
- `git status --porcelain` capture
- `git diff` capture
- changed file list capture

Done when:

- every session has start/end Git metadata
- final code diff can be inspected from DB/export

---

### Phase 4: Filesystem Observer

Build:

- file watcher
- create/modify/delete event capture
- content hashing
- ignore rules

Ignore by default:

- `.git/`
- `node_modules/`
- `.next/`
- `dist/`
- `build/`
- `.venv/`
- `target/`
- large binary files

Done when:

- file changes during a session are visible in event history

---

### Phase 5: Validation Evidence

Build:

- detection of test/build/lint/typecheck commands
- validation event classification
- pass/fail extraction via exit code
- artifact capture for reports

Done when:

- a session summary shows which validation commands passed or failed

---

### Phase 6: Session Inspection

Build:

- `agentrun sessions`
- `agentrun show <session_id>`
- `agentrun export <session_id>`
- JSONL export
- optional HTML report

Done when:

- a developer can inspect a full agent session without opening raw DB tables

---

## 15. Future Capabilities

After MVP, consider:

- enforcing Definition of Done
- blocking session completion if tests fail
- comparing agent behavior across Codex and Claude
- detecting repeated failure loops
- replaying sessions
- generating task audit reports
- summarizing agent execution paths
- integrating with CI
- uploading redacted session logs to a team server
- building a web UI
- adding MCP-based telemetry
- correlating session events with PRs
- creating agent scorecards

---

## 16. Key Risks

### 16.1 Incomplete Hook Coverage

Native hooks may not expose every action.

Mitigation:

- always use PTY capture
- always use Git/file observation
- store raw transcripts as artifacts when available

### 16.2 Secret Leakage

Prompts, outputs, files, or diffs may contain secrets.

Mitigation:

- redact before persistence
- block known secret patterns
- never persist raw `.env` contents by default
- support allowlist/denylist config

### 16.3 Transcript Instability

Agent transcript formats may change.

Mitigation:

- treat transcripts as optional artifacts
- do not depend on transcripts as canonical storage
- use hooks and event log as canonical source

### 16.4 Noisy Logs

PTY and filesystem logs can be verbose.

Mitigation:

- use event types
- compress artifacts
- ignore known generated directories
- support retention policies

### 16.5 Session Correlation

Hooks must map to the correct wrapper session.

Mitigation:

- inject `AGENTRUN_SESSION_ID`
- include session ID in hook environment
- include session ID in hook payload
- reject uncorrelated events or store them separately

---

## 17. Configuration File

Example config:

```yaml
database:
  type: sqlite
  path: .agentrun/agentrun.db

capture:
  terminal: true
  hooks: true
  git: true
  filesystem: true
  validation: true

redaction:
  enabled: true
  redact_env_files: true
  redact_known_tokens: true

ignore:
  paths:
    - .git
    - node_modules
    - .next
    - dist
    - build
    - .venv
    - target

export:
  default_format: jsonl
```

---

## 18. Final Success Criteria

A developer must be able to answer these questions for any recorded session:

- What did the user ask?
- Which agent was used?
- Which repo was touched?
- What was the initial Git state?
- What commands ran?
- What tool calls happened?
- What files changed?
- What outputs were produced?
- What errors occurred?
- What approvals were requested?
- What validation commands ran?
- Which validations passed or failed?
- What was the final Git diff?
- Why was the session considered complete?

---

## 19. Product Principle

The recorder should be boring, durable, and append-only.

Do not depend on agent goodwill.

Do not depend on memory.

Do not depend on final summaries.

Record observable facts as events, preserve artifacts, and make completion auditable.
