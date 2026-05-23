package parse

import (
	"encoding/json"
	"time"
)

// ParseClaudeLine converts one JSONL line from ~/.claude/projects/<…>/*.jsonl
// into our normalized (Session updates, Event) pair.
//
// We never error out hard — unknown line types become a RoleSystem event with
// the raw payload. The caller decides whether to keep or skip them.
//
// The Session pointer is updated IN-PLACE — fields are filled as encountered.
// The caller starts with a Session whose Agent + TranscriptPath are set and
// passes the same pointer for every line in the file.
//
// Returns ok=false ONLY for blank lines or unparseable JSON; those should be
// silently skipped.
func ParseClaudeLine(line []byte, seq int, sess *Session) (Event, bool) {
	if len(line) == 0 {
		return Event{}, false
	}

	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}

	ev := Event{
		Seq:     seq,
		Payload: append([]byte(nil), line...), // defensive copy
	}
	if ts, ok := raw["timestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.Ts = t
		}
	}

	// Capture session-level metadata opportunistically.
	if cwd, ok := raw["cwd"].(string); ok && sess.Cwd == "" {
		sess.Cwd = cwd
	}
	if id, ok := raw["sessionId"].(string); ok && sess.UUID == "" {
		sess.UUID = id
	}

	typ, _ := raw["type"].(string)
	switch typ {
	case "user":
		ev.Role = RoleUser
		ev.Content = extractClaudeMessageText(raw)
	case "assistant":
		// Assistant lines can carry text content, tool_use, or both.
		// If both are present we still emit one event with role=assistant
		// and content = the text portion; tool_use becomes its own
		// downstream event when we see it elsewhere. Actually Claude
		// embeds tool_use inside the assistant content array — we split.
		if name, input, ok := extractClaudeToolUse(raw); ok {
			ev.Role = RoleToolUse
			ev.ToolName = name
			ev.Content = input
			break
		}
		ev.Role = RoleAssistant
		ev.Content = extractClaudeMessageText(raw)
		if model := claudeModel(raw); model != "" && sess.Model == "" {
			sess.Model = model
		}
	default:
		ev.Role = RoleSystem
		// content stays empty; payload has the full original line for forensics
	}

	// Session lifecycle bookkeeping.
	if sess.StartedAt.IsZero() || (!ev.Ts.IsZero() && ev.Ts.Before(sess.StartedAt)) {
		if !ev.Ts.IsZero() {
			sess.StartedAt = ev.Ts
		}
	}
	if !ev.Ts.IsZero() && ev.Ts.After(sess.EndedAt) {
		sess.EndedAt = ev.Ts
	}
	return ev, true
}

// extractClaudeMessageText pulls the human-readable text out of a Claude
// `user` or `assistant` line. content can be either:
//   - a plain string ("test log")
//   - an array of typed blocks: [{"type":"text","text":"..."}, ...]
//
// We concatenate text blocks with newlines. Tool_use blocks are ignored
// here (extractClaudeToolUse handles them separately).
func extractClaudeMessageText(raw map[string]any) string {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	switch c := msg["content"].(type) {
	case string:
		return c
	case []any:
		var out string
		for _, block := range c {
			b, _ := block.(map[string]any)
			if b == nil {
				continue
			}
			if t, _ := b["type"].(string); t != "text" {
				continue
			}
			if text, _ := b["text"].(string); text != "" {
				if out != "" {
					out += "\n"
				}
				out += text
			}
		}
		return out
	}
	return ""
}

// extractClaudeToolUse returns the first tool_use block found in an
// assistant line. ok=false means no tool was invoked in this line.
func extractClaudeToolUse(raw map[string]any) (name, inputJSON string, ok bool) {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return "", "", false
	}
	blocks, _ := msg["content"].([]any)
	for _, block := range blocks {
		b, _ := block.(map[string]any)
		if b == nil {
			continue
		}
		if t, _ := b["type"].(string); t != "tool_use" {
			continue
		}
		name, _ = b["name"].(string)
		if input, ok := b["input"]; ok {
			if buf, err := json.Marshal(input); err == nil {
				inputJSON = string(buf)
			}
		}
		return name, inputJSON, true
	}
	return "", "", false
}

func claudeModel(raw map[string]any) string {
	msg, _ := raw["message"].(map[string]any)
	if msg == nil {
		return ""
	}
	m, _ := msg["model"].(string)
	return m
}
