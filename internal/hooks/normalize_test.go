package hooks

import (
	"reflect"
	"testing"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		input      string
		source     string
		typ        string
		counter    string
		knownEvent bool
	}{
		{"SessionStart", "hook", "hook.session_start", "", true},
		{"UserPromptSubmit", "hook", "user.prompt", "user_prompts", true},
		{"PreToolUse", "hook", "tool.pre_use", "tool_calls", true},
		{"PostToolUse", "hook", "tool.post_use", "", true},
		{"PostToolUseFailure", "hook", "tool.failed", "errors", true},
		{"PermissionRequest", "hook", "approval.requested", "approvals_request", true},
		{"PermissionDenied", "hook", "approval.responded", "approvals_denied", true},
		{"Notification", "hook", "notification", "", true},
		{"Stop", "hook", "response.stopped", "", true},
		{"PreCompact", "hook", "context.pre_compact", "", true},
		{"SessionEnd", "hook", "hook.session_end", "", true},
		{"SubagentStart", "hook", "subagent.started", "tool_calls", true},
		{"SubagentStop", "hook", "subagent.stopped", "", true},
		{"TaskCreated", "hook", "task.created", "", true},
		{"TaskCompleted", "hook", "task.completed", "", true},
		{"UnknownEvent42", "hook", "hook.UnknownEvent42", "", false},
		{"", "hook", "hook.", "", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.input, func(t *testing.T) {
			got := Normalize(tc.input)
			if got.Source != tc.source {
				t.Errorf("Source: got %q, want %q", got.Source, tc.source)
			}
			if got.Type != tc.typ {
				t.Errorf("Type: got %q, want %q", got.Type, tc.typ)
			}
			if got.Counter != tc.counter {
				t.Errorf("Counter: got %q, want %q", got.Counter, tc.counter)
			}
			if got.KnownEvent != tc.knownEvent {
				t.Errorf("KnownEvent: got %v, want %v", got.KnownEvent, tc.knownEvent)
			}
		})
	}
}

func TestMVPEvents(t *testing.T) {
	want := []string{
		"SessionStart",
		"UserPromptSubmit",
		"PreToolUse",
		"PostToolUse",
		"PostToolUseFailure",
		"PermissionRequest",
		"PermissionDenied",
		"Notification",
		"Stop",
		"PreCompact",
		"SessionEnd",
		"SubagentStart",
		"SubagentStop",
		"TaskCreated",
		"TaskCompleted",
	}

	got := MVPEvents()
	if len(got) != len(want) {
		t.Fatalf("MVPEvents() returned %d entries, want %d", len(got), len(want))
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MVPEvents() = %v, want %v", got, want)
	}
}
