package hooks

// Mapping is the result of resolving one Claude hook event name.
type Mapping struct {
	Source     string // always "hook" today
	Type       string // e.g. "tool.pre_use"
	Counter    string // session_summary column name, or "" for no increment
	KnownEvent bool   // true if this event is in our MVP table
}

// mvpTable is the authoritative list of Claude hook events we handle in Phase 2.
// Order is stable; MVPEvents() returns names in this order.
var mvpTable = []struct {
	EventName string
	Type      string
	Counter   string
}{
	{"SessionStart", "hook.session_start", ""},
	{"UserPromptSubmit", "user.prompt", "user_prompts"},
	{"PreToolUse", "tool.pre_use", "tool_calls"},
	{"PostToolUse", "tool.post_use", ""},
	{"PostToolUseFailure", "tool.failed", "errors"},
	{"PermissionRequest", "approval.requested", "approvals_request"},
	{"PermissionDenied", "approval.responded", "approvals_denied"},
	{"Notification", "notification", ""},
	{"Stop", "response.stopped", ""},
	{"PreCompact", "context.pre_compact", ""},
	{"SessionEnd", "hook.session_end", ""},
}

// Normalize converts a Claude hook event name (e.g. "PreToolUse") into our
// canonical event-type taxonomy and the summary counter to increment.
//
// Unknown events return KnownEvent=false with Type="hook.<eventName>" and
// no counter. This is the forward-compat path for new Anthropic events.
func Normalize(eventName string) Mapping {
	// Linear scan: 11 entries — cheaper than a map at this size and avoids
	// package-init allocation.
	for _, row := range mvpTable {
		if row.EventName == eventName {
			return Mapping{
				Source:     "hook",
				Type:       row.Type,
				Counter:    row.Counter,
				KnownEvent: true,
			}
		}
	}
	return Mapping{
		Source:     "hook",
		Type:       "hook." + eventName,
		Counter:    "",
		KnownEvent: false,
	}
}

// MVPEvents returns the list of Claude event names we register hooks for.
// Order is stable so unit tests can diff against it.
func MVPEvents() []string {
	out := make([]string, len(mvpTable))
	for i, row := range mvpTable {
		out[i] = row.EventName
	}
	return out
}
