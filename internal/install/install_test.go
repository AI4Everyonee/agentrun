package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFakeHome redirects HOME (and USERPROFILE) to a fresh temp dir for the
// duration of the test, so install/uninstall touch a sandbox rather than the
// real ~/.claude or ~/.codex.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func TestInstall_FromScratch_CreatesBothFiles(t *testing.T) {
	home := withFakeHome(t)

	res, err := Install("/usr/local/bin/agentrun")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	wantClaude := filepath.Join(home, ".claude", "settings.json")
	wantCodex := filepath.Join(home, ".codex", "config.toml")

	if res.Claude.Path != wantClaude {
		t.Errorf("Claude.Path = %q, want %q", res.Claude.Path, wantClaude)
	}
	if res.Claude.Action != "created" {
		t.Errorf("Claude.Action = %q, want created", res.Claude.Action)
	}
	if res.Claude.Backup != "" {
		t.Errorf("Claude.Backup should be empty when no prior file existed; got %q", res.Claude.Backup)
	}

	if res.Codex.Path != wantCodex {
		t.Errorf("Codex.Path = %q, want %q", res.Codex.Path, wantCodex)
	}
	if res.Codex.Action != "created" {
		t.Errorf("Codex.Action = %q, want created", res.Codex.Action)
	}

	// Verify Claude file has the 11 events as keys under "hooks".
	cb, err := os.ReadFile(wantClaude)
	if err != nil {
		t.Fatalf("read claude: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(cb, &parsed); err != nil {
		t.Fatalf("parse claude json: %v", err)
	}
	hooksObj, ok := parsed["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks key missing or wrong type in %s", wantClaude)
	}
	for _, ev := range []string{
		"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
		"PostToolUseFailure", "PermissionRequest", "PermissionDenied",
		"Notification", "Stop", "PreCompact", "SessionEnd",
	} {
		if _, ok := hooksObj[ev]; !ok {
			t.Errorf("claude hooks missing event %q", ev)
		}
	}

	// Verify Codex file has markers + every codex event.
	tb, err := os.ReadFile(wantCodex)
	if err != nil {
		t.Fatalf("read codex: %v", err)
	}
	codex := string(tb)
	if !strings.Contains(codex, codexMarkerBegin) {
		t.Errorf("codex file missing BEGIN marker")
	}
	if !strings.Contains(codex, codexMarkerEnd) {
		t.Errorf("codex file missing END marker")
	}
	for _, ev := range []string{"SessionStart", "PreToolUse", "PostToolUse", "Stop"} {
		if !strings.Contains(codex, "[[hooks."+ev+"]]") {
			t.Errorf("codex file missing event header for %s", ev)
		}
	}
}

func TestInstall_PreservesUserHooks(t *testing.T) {
	home := withFakeHome(t)
	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}

	original := map[string]any{
		"model": "claude-sonnet-4-7",
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/usr/local/bin/user-policy.sh",
							"timeout": 30,
						},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(original, "", "  ")
	if err := os.WriteFile(claudePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install("/usr/local/bin/agentrun"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	parsed := readJSON(t, claudePath)

	// User's top-level "model" key must be preserved.
	if parsed["model"] != "claude-sonnet-4-7" {
		t.Errorf("user model setting lost: %v", parsed["model"])
	}

	preTool, ok := parsed["hooks"].(map[string]any)["PreToolUse"].([]any)
	if !ok {
		t.Fatalf("PreToolUse not an array")
	}
	// Should now contain BOTH the user's entry and ours.
	if len(preTool) < 2 {
		t.Errorf("PreToolUse has %d entries; expected at least user + ours", len(preTool))
	}

	// Verify the user-policy.sh entry is still present.
	foundUser := false
	foundAgentrun := false
	for _, g := range preTool {
		grp, _ := g.(map[string]any)
		cmds, _ := grp["hooks"].([]any)
		for _, c := range cmds {
			cmd, _ := c.(map[string]any)
			cmdStr, _ := cmd["command"].(string)
			if strings.Contains(cmdStr, "user-policy.sh") {
				foundUser = true
			}
			if strings.Contains(cmdStr, "agentrun hook claude PreToolUse") {
				foundAgentrun = true
			}
		}
	}
	if !foundUser {
		t.Errorf("user's PreToolUse hook was dropped")
	}
	if !foundAgentrun {
		t.Errorf("agentrun's PreToolUse hook was not added")
	}
}

func TestInstall_Idempotent_NoDuplicatesOnSecondRun(t *testing.T) {
	home := withFakeHome(t)
	claudePath := filepath.Join(home, ".claude", "settings.json")

	if _, err := Install("/usr/local/bin/agentrun"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if _, err := Install("/usr/local/bin/agentrun"); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	parsed := readJSON(t, claudePath)
	hooksObj := parsed["hooks"].(map[string]any)
	preTool := hooksObj["PreToolUse"].([]any)
	// We expect exactly one agentrun entry.
	count := 0
	for _, g := range preTool {
		grp, _ := g.(map[string]any)
		cmds, _ := grp["hooks"].([]any)
		for _, c := range cmds {
			cmd, _ := c.(map[string]any)
			cmdStr, _ := cmd["command"].(string)
			if strings.Contains(cmdStr, "agentrun hook claude PreToolUse") {
				count++
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 agentrun PreToolUse entry after two installs, got %d", count)
	}
}

func TestInstall_BackupsExistingFiles(t *testing.T) {
	home := withFakeHome(t)
	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(`{"model":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install("/usr/local/bin/agentrun")
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Claude.Backup == "" {
		t.Errorf("expected backup file when existing file present")
	}
	if !strings.HasPrefix(res.Claude.Backup, claudePath+".bak.") {
		t.Errorf("backup path %q does not match expected pattern", res.Claude.Backup)
	}
	if _, err := os.Stat(res.Claude.Backup); err != nil {
		t.Errorf("backup file does not exist: %v", err)
	}
}

func TestUninstall_RemovesEntriesPreservesUserHooks(t *testing.T) {
	home := withFakeHome(t)
	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pre-existing user hook.
	original := map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "/usr/local/bin/user-policy.sh",
						},
					},
				},
			},
		},
	}
	b, _ := json.MarshalIndent(original, "", "  ")
	if err := os.WriteFile(claudePath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install("/usr/local/bin/agentrun"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	parsed := readJSON(t, claudePath)
	hooksObj := parsed["hooks"].(map[string]any)
	preTool := hooksObj["PreToolUse"].([]any)
	for _, g := range preTool {
		grp, _ := g.(map[string]any)
		cmds, _ := grp["hooks"].([]any)
		for _, c := range cmds {
			cmd, _ := c.(map[string]any)
			cmdStr, _ := cmd["command"].(string)
			if strings.Contains(cmdStr, "agentrun hook claude") {
				t.Errorf("agentrun entry survived uninstall: %q", cmdStr)
			}
		}
	}
	// User hook must still be there.
	found := false
	for _, g := range preTool {
		grp, _ := g.(map[string]any)
		cmds, _ := grp["hooks"].([]any)
		for _, c := range cmds {
			cmd, _ := c.(map[string]any)
			cmdStr, _ := cmd["command"].(string)
			if strings.Contains(cmdStr, "user-policy.sh") {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("user's policy hook was removed during uninstall")
	}
}

func TestUninstall_CodexRemovesMarkerBlock(t *testing.T) {
	home := withFakeHome(t)
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// User has some pre-existing config.
	userConfig := "model = \"o3\"\n\n[mcp_servers.context7]\nenabled = true\n"
	if err := os.WriteFile(codexPath, []byte(userConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Install("/usr/local/bin/agentrun"); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// After install, codex file must have the markers AND the user content.
	after, _ := os.ReadFile(codexPath)
	if !strings.Contains(string(after), `model = "o3"`) {
		t.Errorf("user content lost after install")
	}
	if !strings.Contains(string(after), codexMarkerBegin) {
		t.Errorf("marker begin missing")
	}

	if _, err := Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	after2, _ := os.ReadFile(codexPath)
	if strings.Contains(string(after2), codexMarkerBegin) {
		t.Errorf("marker begin survived uninstall")
	}
	if !strings.Contains(string(after2), `model = "o3"`) {
		t.Errorf("user content lost after uninstall")
	}
}

func TestUninstall_NoOpWhenFilesAbsent(t *testing.T) {
	withFakeHome(t)
	res, err := Uninstall()
	if err != nil {
		t.Fatalf("Uninstall on empty home returned error: %v", err)
	}
	if res.Claude.Action != "skipped" {
		t.Errorf("Claude.Action = %q, want skipped", res.Claude.Action)
	}
	if res.Codex.Action != "skipped" {
		t.Errorf("Codex.Action = %q, want skipped", res.Codex.Action)
	}
}

func TestInstall_BadJSON_RefusesToOverwrite(t *testing.T) {
	home := withFakeHome(t)
	claudePath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(`{ this is not json`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Install("/usr/local/bin/agentrun")
	if err == nil {
		t.Errorf("expected Install to error on malformed JSON file")
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}
