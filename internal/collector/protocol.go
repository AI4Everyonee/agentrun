// Package collector implements a long-lived hook receiver process that accepts
// events from agentrun hook subprocesses over a Unix domain socket, eliminating
// the per-hook fork+DB-open cost.
//
// Wire format: one JSON object per line. The client writes a Request, the
// server responds with a single byte (0x00 = ok, 0x01 = error) followed by a newline.
package collector

import "encoding/json"

// Request is the message sent from the hook subcommand to the collector server.
type Request struct {
	Agent     string          `json:"agent"`      // "claude" | "codex"
	Event     string          `json:"event"`      // "PreToolUse" etc.
	SessionID string          `json:"session_id"` // env-resolved (may be empty for native)
	Payload   json.RawMessage `json:"payload"`    // the hook stdin JSON, raw
	Native    bool            `json:"native"`     // true if wrapper didn't set AGENTRUN_SESSION_ID
}

// Response byte codes.
const (
	ResponseOK  byte = 0x00
	ResponseErr byte = 0x01
)
