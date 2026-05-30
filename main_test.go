package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of f and returns what was written.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		done <- buf.String()
	}()

	f()
	w.Close()
	return <-done
}

func TestDefaultRoots_Defaults(t *testing.T) {
	// Ensure the env vars are unset so we get the home-based defaults.
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "")
	t.Setenv("AGENTRUN_CODEX_ROOT", "")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	rt, err := defaultRoots()
	if err != nil {
		t.Fatalf("defaultRoots: %v", err)
	}
	wantClaude := filepath.Join(home, ".claude", "projects")
	wantCodex := filepath.Join(home, ".codex", "sessions")
	if rt.Claude != wantClaude {
		t.Errorf("Claude root = %q, want %q", rt.Claude, wantClaude)
	}
	if rt.Codex != wantCodex {
		t.Errorf("Codex root = %q, want %q", rt.Codex, wantCodex)
	}
}

func TestDefaultRoots_ClaudeOverride(t *testing.T) {
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "/custom/claude")
	t.Setenv("AGENTRUN_CODEX_ROOT", "")

	home, _ := os.UserHomeDir()
	rt, err := defaultRoots()
	if err != nil {
		t.Fatalf("defaultRoots: %v", err)
	}
	if rt.Claude != "/custom/claude" {
		t.Errorf("Claude root = %q, want /custom/claude", rt.Claude)
	}
	wantCodex := filepath.Join(home, ".codex", "sessions")
	if rt.Codex != wantCodex {
		t.Errorf("Codex root = %q, want %q", rt.Codex, wantCodex)
	}
}

func TestDefaultRoots_CodexOverride(t *testing.T) {
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "")
	t.Setenv("AGENTRUN_CODEX_ROOT", "/custom/codex")

	home, _ := os.UserHomeDir()
	rt, err := defaultRoots()
	if err != nil {
		t.Fatalf("defaultRoots: %v", err)
	}
	wantClaude := filepath.Join(home, ".claude", "projects")
	if rt.Claude != wantClaude {
		t.Errorf("Claude root = %q, want %q", rt.Claude, wantClaude)
	}
	if rt.Codex != "/custom/codex" {
		t.Errorf("Codex root = %q, want /custom/codex", rt.Codex)
	}
}

func TestDefaultRoots_BothOverrides(t *testing.T) {
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "/alt/claude")
	t.Setenv("AGENTRUN_CODEX_ROOT", "/alt/codex")

	rt, err := defaultRoots()
	if err != nil {
		t.Fatalf("defaultRoots: %v", err)
	}
	if rt.Claude != "/alt/claude" {
		t.Errorf("Claude root = %q, want /alt/claude", rt.Claude)
	}
	if rt.Codex != "/alt/codex" {
		t.Errorf("Codex root = %q, want /alt/codex", rt.Codex)
	}
}

func TestCmdPaths_PrintsBothRoots(t *testing.T) {
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "/my/claude")
	t.Setenv("AGENTRUN_CODEX_ROOT", "/my/codex")

	out := captureStdout(t, func() {
		if err := cmdPaths(nil); err != nil {
			t.Errorf("cmdPaths returned error: %v", err)
		}
	})

	if !strings.Contains(out, "claude: /my/claude") {
		t.Errorf("output missing claude line; got:\n%s", out)
	}
	if !strings.Contains(out, "codex:  /my/codex") {
		t.Errorf("output missing codex line; got:\n%s", out)
	}
}

func TestCmdPaths_DefaultRoots(t *testing.T) {
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "")
	t.Setenv("AGENTRUN_CODEX_ROOT", "")

	home, _ := os.UserHomeDir()
	wantClaude := fmt.Sprintf("claude: %s", filepath.Join(home, ".claude", "projects"))
	wantCodex := fmt.Sprintf("codex:  %s", filepath.Join(home, ".codex", "sessions"))

	out := captureStdout(t, func() {
		if err := cmdPaths(nil); err != nil {
			t.Errorf("cmdPaths returned error: %v", err)
		}
	})

	if !strings.Contains(out, wantClaude) {
		t.Errorf("output missing %q; got:\n%s", wantClaude, out)
	}
	if !strings.Contains(out, wantCodex) {
		t.Errorf("output missing %q; got:\n%s", wantCodex, out)
	}
}

func TestCmdPaths_RejectsExtraArgs(t *testing.T) {
	err := cmdPaths([]string{"extra"})
	if err != errUsage {
		t.Errorf("cmdPaths(extra) = %v, want errUsage", err)
	}
}

func TestRunDispatch_PathsCommand(t *testing.T) {
	t.Setenv("AGENTRUN_CLAUDE_ROOT", "/dispatch/claude")
	t.Setenv("AGENTRUN_CODEX_ROOT", "/dispatch/codex")

	out := captureStdout(t, func() {
		if err := run([]string{"paths"}); err != nil {
			t.Errorf("run(paths) returned error: %v", err)
		}
	})

	if !strings.Contains(out, "/dispatch/claude") {
		t.Errorf("dispatch output missing claude root; got:\n%s", out)
	}
	if !strings.Contains(out, "/dispatch/codex") {
		t.Errorf("dispatch output missing codex root; got:\n%s", out)
	}
}
