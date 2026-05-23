package hooks

import (
	"reflect"
	"strings"
	"testing"
)

func TestCodexMVPEvents_StableOrder(t *testing.T) {
	want := []string{
		"SessionStart",
		"UserPromptSubmit",
		"PreToolUse",
		"PostToolUse",
		"PermissionRequest",
		"Stop",
		"PreCompact",
	}
	got := CodexMVPEvents()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CodexMVPEvents() = %v, want %v", got, want)
	}
}

func TestCodexMVPEvents_ReturnsCopy(t *testing.T) {
	first := CodexMVPEvents()
	first[0] = "MUTATED"
	second := CodexMVPEvents()
	if second[0] != "SessionStart" {
		t.Errorf("second call returned mutated slice: got %q at [0]", second[0])
	}
}

func TestBuildCodexHooksTOML_ContainsEveryEvent(t *testing.T) {
	out := BuildCodexHooksTOML("/usr/local/bin/agentrun")

	for _, ev := range CodexMVPEvents() {
		header := "[[hooks." + ev + "]]"
		if !strings.Contains(out, header) {
			t.Errorf("output missing event header %q", header)
		}
		hookHeader := "[[hooks." + ev + ".hooks]]"
		if !strings.Contains(out, hookHeader) {
			t.Errorf("output missing hook entry header %q", hookHeader)
		}
		wantCmd := `command = "/usr/local/bin/agentrun hook codex ` + ev + `"`
		if !strings.Contains(out, wantCmd) {
			t.Errorf("output missing command line for %s; want substring %q", ev, wantCmd)
		}
	}

	// Every entry should carry our 10-second timeout.
	if got := strings.Count(out, "timeout = 10"); got != len(CodexMVPEvents()) {
		t.Errorf("timeout count = %d, want %d", got, len(CodexMVPEvents()))
	}

	// type = "command" should appear once per event.
	if got := strings.Count(out, `type = "command"`); got != len(CodexMVPEvents()) {
		t.Errorf("type=command count = %d, want %d", got, len(CodexMVPEvents()))
	}
}

func TestBuildCodexHooksTOML_AgentArgvIsCodex(t *testing.T) {
	// Sanity: agentrun hook command argv must include "codex" so the hook
	// process attributes events to the codex agent (not claude).
	out := BuildCodexHooksTOML("/usr/local/bin/agentrun")
	if !strings.Contains(out, "agentrun hook codex ") {
		t.Errorf("expected 'agentrun hook codex' in output, got:\n%s", out)
	}
	if strings.Contains(out, "agentrun hook claude ") {
		t.Errorf("Codex TOML should NOT contain 'agentrun hook claude'; got:\n%s", out)
	}
}

func TestBuildCodexHooksTOML_EscapesPathWithBackslash(t *testing.T) {
	// Backslash in path must be escaped for TOML basic strings.
	out := BuildCodexHooksTOML(`/path\with\backslash/agentrun`)
	if !strings.Contains(out, `/path\\with\\backslash/agentrun`) {
		t.Errorf("backslashes not TOML-escaped; output:\n%s", out)
	}
}

func TestBuildCodexHooksTOML_EventsAreSubsetOfMVP(t *testing.T) {
	mvp := map[string]bool{}
	for _, ev := range MVPEvents() {
		mvp[ev] = true
	}
	for _, ev := range CodexMVPEvents() {
		if !mvp[ev] {
			t.Errorf("Codex event %q is not in MVPEvents() — hook events will fall through to type=hook.%s", ev, ev)
		}
	}
}
