// Package parse converts agent JSONL files into normalized Session + Event values.
//
// The agents (Claude Code, Codex) each write their own JSONL transcripts to a
// well-known directory:
//
//	Claude → ~/.claude/projects/<derived>/<session-uuid>.jsonl
//	Codex  → ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl
//
// Their schemas differ. This package's job is to flatten both into the same
// shape so the watcher and the DB don't need to care which agent produced
// them.
package parse

import "time"

// Agent identifies which CLI produced the transcript.
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// Session is the per-file metadata we extract. Most fields can stay zero —
// the watcher fills them progressively as it encounters relevant lines.
type Session struct {
	Agent          Agent
	UUID           string // the agent's own session UUID
	UserName       string
	Cwd            string
	Model          string
	StartedAt      time.Time
	EndedAt        time.Time
	TranscriptPath string // the source JSONL file path
}

// Event is one row in the events table. payload is the raw original JSONL
// line as bytes — preserved verbatim so consumers can drill deeper than
// our extracted fields when needed.
type Event struct {
	Seq      int       // 1-based line number in the source JSONL
	Ts       time.Time // timestamp from the line itself, falls back to file mtime
	Role     Role
	ToolName string // empty unless Role is ToolUse or ToolResult
	Content  string // extracted human-readable text (may be empty)
	Payload  []byte // the raw JSONL line, untouched
}

// Role categorizes an event for downstream filtering and skill mining.
// We deliberately use a small, fixed set rather than the agents' native
// type strings — those are agent-specific.
type Role string

const (
	RoleUser       Role = "user"        // a user prompt
	RoleAssistant  Role = "assistant"   // the model's natural-language reply
	RoleToolUse    Role = "tool_use"    // the model invoked a tool
	RoleToolResult Role = "tool_result" // a tool returned output to the model
	RoleSystem     Role = "system"      // metadata, lifecycle, anything else
)
