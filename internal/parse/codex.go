package parse

import (
	"encoding/json"
	"time"
)

// ParseCodexLine converts one JSONL line from ~/.codex/sessions/<…>/*.jsonl
// into our normalized (Session updates, Event) pair. Mirror of ParseClaudeLine
// but for Codex's different schema.
//
// Codex line shape:
//
//	{"timestamp": "...", "type": "<outer>", "payload": {"type": "<inner>", ...}}
//
// outer ∈ {session_meta, turn_context, event_msg, response_item}
// inner varies — we mostly care about:
//
//	event_msg.user_message       → role=user
//	event_msg.agent_message      → role=assistant
//	response_item.function_call  → role=tool_use
//	response_item.function_call_output → role=tool_result
//	response_item.message (role=assistant/developer) → role=assistant/system
//	session_meta                 → metadata only, no event emitted; sets session.Model/UUID
//	event_msg.task_started/complete → role=system
func ParseCodexLine(line []byte, seq int, sess *Session) (Event, bool) {
	if len(line) == 0 {
		return Event{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}

	ev := Event{
		Seq:     seq,
		Payload: append([]byte(nil), line...),
	}
	if ts, ok := raw["timestamp"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.Ts = t
		}
	}

	outerType, _ := raw["type"].(string)
	payload, _ := raw["payload"].(map[string]any)

	// Pull session metadata when we see it.
	if outerType == "session_meta" && payload != nil {
		if id, ok := payload["id"].(string); ok && sess.UUID == "" {
			sess.UUID = id
		}
		if cwd, ok := payload["cwd"].(string); ok && sess.Cwd == "" {
			sess.Cwd = cwd
		}
		// model is sometimes nested under `model.id`, sometimes flat.
		if m, ok := payload["model"].(string); ok && sess.Model == "" {
			sess.Model = m
		}
	}
	// turn_context can carry the model too.
	if outerType == "turn_context" && payload != nil && sess.Model == "" {
		if m, ok := payload["model"].(string); ok {
			sess.Model = m
		}
	}

	innerType := ""
	if payload != nil {
		innerType, _ = payload["type"].(string)
	}

	switch {
	case outerType == "event_msg" && innerType == "user_message":
		ev.Role = RoleUser
		if payload != nil {
			ev.Content, _ = payload["message"].(string)
			if ev.Content == "" {
				ev.Content, _ = payload["text"].(string)
			}
		}

	case outerType == "event_msg" && innerType == "agent_message":
		ev.Role = RoleAssistant
		if payload != nil {
			ev.Content, _ = payload["message"].(string)
			if ev.Content == "" {
				ev.Content, _ = payload["text"].(string)
			}
		}

	case outerType == "response_item" && innerType == "function_call":
		ev.Role = RoleToolUse
		if payload != nil {
			ev.ToolName, _ = payload["name"].(string)
			// arguments may be a string (JSON-encoded) or already-parsed object.
			if args, ok := payload["arguments"].(string); ok {
				ev.Content = args
			} else if argsObj, ok := payload["arguments"].(map[string]any); ok {
				if b, err := json.Marshal(argsObj); err == nil {
					ev.Content = string(b)
				}
			}
		}

	case outerType == "response_item" && innerType == "function_call_output":
		ev.Role = RoleToolResult
		if payload != nil {
			ev.ToolName, _ = payload["name"].(string)
			ev.Content, _ = payload["output"].(string)
		}

	case outerType == "response_item" && innerType == "message":
		// Conversational message — role lives inside payload.
		role := ""
		if payload != nil {
			role, _ = payload["role"].(string)
		}
		switch role {
		case "user":
			ev.Role = RoleUser
		case "assistant":
			ev.Role = RoleAssistant
		default:
			// "developer", "system", etc. — these are scaffolding messages
			// the agent injects (sandbox notes, role primers). Keep them as system.
			ev.Role = RoleSystem
		}
		ev.Content = extractCodexMessageText(payload)

	default:
		ev.Role = RoleSystem
	}

	if sess.StartedAt.IsZero() && !ev.Ts.IsZero() {
		sess.StartedAt = ev.Ts
	}
	if !ev.Ts.IsZero() && ev.Ts.After(sess.EndedAt) {
		sess.EndedAt = ev.Ts
	}
	return ev, true
}

// extractCodexMessageText pulls text from a response_item.message payload.
// Content is an array of typed blocks like Claude's.
func extractCodexMessageText(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	blocks, _ := payload["content"].([]any)
	var out string
	for _, block := range blocks {
		b, _ := block.(map[string]any)
		if b == nil {
			continue
		}
		// Codex uses "input_text" and "output_text" as block types.
		typ, _ := b["type"].(string)
		if typ != "input_text" && typ != "output_text" && typ != "text" {
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
