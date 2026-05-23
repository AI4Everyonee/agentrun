package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testHookCommand mirrors hookCommand for test decoding.
type testHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// testHookGroup mirrors hookGroup for test decoding, with *string matcher
// so we can distinguish nil (omitted) from pointer-to-empty (emitted).
type testHookGroup struct {
	Matcher *string           `json:"matcher,omitempty"`
	Hooks   []testHookCommand `json:"hooks"`
}

// testSettingsFile mirrors settingsFile for test decoding.
type testSettingsFile struct {
	Hooks map[string][]testHookGroup `json:"hooks"`
}

func TestGenerateSettings_Roundtrip(t *testing.T) {
	tmp := t.TempDir()
	path, err := GenerateSettings(tmp, "s_test", "/usr/local/bin/agentrun")
	if err != nil {
		t.Fatalf("GenerateSettings error: %v", err)
	}

	wantPath := filepath.Join(tmp, "sessions", "s_test", "hooks.json")
	if path != wantPath {
		t.Errorf("returned path = %q, want %q", path, wantPath)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var out testSettingsFile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if len(out.Hooks) != 11 {
		t.Errorf("len(Hooks) = %d, want 11", len(out.Hooks))
	}

	// Verify every MVP event is present with correct structure.
	for _, ev := range MVPEvents() {
		groups, ok := out.Hooks[ev]
		if !ok {
			t.Errorf("Hooks[%q] missing", ev)
			continue
		}
		if len(groups) != 1 {
			t.Errorf("Hooks[%q]: got %d groups, want 1", ev, len(groups))
			continue
		}
		if len(groups[0].Hooks) != 1 {
			t.Errorf("Hooks[%q][0].Hooks: got %d entries, want 1", ev, len(groups[0].Hooks))
			continue
		}
		wantCmd := "/usr/local/bin/agentrun hook " + ev
		if groups[0].Hooks[0].Command != wantCmd {
			t.Errorf("Hooks[%q][0].Hooks[0].Command = %q, want %q", ev, groups[0].Hooks[0].Command, wantCmd)
		}
		if groups[0].Hooks[0].Timeout != 10 {
			t.Errorf("Hooks[%q][0].Hooks[0].Timeout = %d, want 10", ev, groups[0].Hooks[0].Timeout)
		}
	}

	// Verify matcher rules.
	preToolUse := out.Hooks["PreToolUse"]
	if len(preToolUse) == 0 {
		t.Fatal("Hooks[PreToolUse] missing")
	}
	if preToolUse[0].Matcher == nil {
		t.Error("Hooks[PreToolUse][0].Matcher should be non-nil (pointing to empty string)")
	} else if *preToolUse[0].Matcher != "" {
		t.Errorf("Hooks[PreToolUse][0].Matcher = %q, want empty string", *preToolUse[0].Matcher)
	}

	userPrompt := out.Hooks["UserPromptSubmit"]
	if len(userPrompt) == 0 {
		t.Fatal("Hooks[UserPromptSubmit] missing")
	}
	if userPrompt[0].Matcher != nil {
		t.Errorf("Hooks[UserPromptSubmit][0].Matcher should be nil (omitted), got %q", *userPrompt[0].Matcher)
	}

	stop := out.Hooks["Stop"]
	if len(stop) == 0 {
		t.Fatal("Hooks[Stop] missing")
	}
	if stop[0].Matcher != nil {
		t.Errorf("Hooks[Stop][0].Matcher should be nil (omitted), got %q", *stop[0].Matcher)
	}
}

func TestGenerateSettings_ShellQuoting(t *testing.T) {
	tmp := t.TempDir()
	_, err := GenerateSettings(tmp, "s_quote", "/path with space/agentrun")
	if err != nil {
		t.Fatalf("GenerateSettings error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmp, "sessions", "s_quote", "hooks.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var out testSettingsFile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	wantCmd := "'/path with space/agentrun' hook PreToolUse"
	got := out.Hooks["PreToolUse"][0].Hooks[0].Command
	if got != wantCmd {
		t.Errorf("command = %q, want %q", got, wantCmd)
	}
}

func TestGenerateSettings_SafePathUnquoted(t *testing.T) {
	tmp := t.TempDir()
	_, err := GenerateSettings(tmp, "s_safe", "/usr/local/bin/agentrun")
	if err != nil {
		t.Fatalf("GenerateSettings error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmp, "sessions", "s_safe", "hooks.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var out testSettingsFile
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	wantCmd := "/usr/local/bin/agentrun hook PreToolUse"
	got := out.Hooks["PreToolUse"][0].Hooks[0].Command
	if got != wantCmd {
		t.Errorf("command = %q, want %q (should NOT be wrapped in quotes)", got, wantCmd)
	}
}

func TestCleanup_RemovesDir(t *testing.T) {
	tmp := t.TempDir()

	_, err := GenerateSettings(tmp, "s_c", "/tmp/agentrun")
	if err != nil {
		t.Fatalf("GenerateSettings: %v", err)
	}

	sessionDir := filepath.Join(tmp, "sessions", "s_c")

	// Confirm the dir was created.
	if _, err := os.Stat(sessionDir); err != nil {
		t.Fatalf("session dir should exist before Cleanup: %v", err)
	}

	// First cleanup removes the dir.
	if err := Cleanup(tmp, "s_c"); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}

	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Errorf("session dir should not exist after Cleanup; stat err = %v", err)
	}

	// Second cleanup is idempotent (os.RemoveAll on missing path returns nil).
	if err := Cleanup(tmp, "s_c"); err != nil {
		t.Errorf("second Cleanup should return nil, got: %v", err)
	}
}

func TestSessionDir_And_SettingsPath(t *testing.T) {
	if got := SessionDir("/x", "y"); got != "/x/sessions/y" {
		t.Errorf("SessionDir = %q, want %q", got, "/x/sessions/y")
	}
	if got := SettingsPath("/x", "y"); got != "/x/sessions/y/hooks.json" {
		t.Errorf("SettingsPath = %q, want %q", got, "/x/sessions/y/hooks.json")
	}
}
