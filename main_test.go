package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultRootsUsesOverrides(t *testing.T) {
	claudeRoot := filepath.Join(t.TempDir(), "claude-projects")
	codexRoot := filepath.Join(t.TempDir(), "codex-sessions")
	t.Setenv("AGENTRUN_CLAUDE_ROOT", claudeRoot)
	t.Setenv("AGENTRUN_CODEX_ROOT", codexRoot)
	t.Setenv("HOME", "")

	rt, err := defaultRoots()
	if err != nil {
		t.Fatalf("defaultRoots() error = %v", err)
	}
	if rt.Claude != claudeRoot {
		t.Fatalf("Claude root = %q, want %q", rt.Claude, claudeRoot)
	}
	if rt.Codex != codexRoot {
		t.Fatalf("Codex root = %q, want %q", rt.Codex, codexRoot)
	}
}

func TestDefaultRootsFallsBackForMissingOverrides(t *testing.T) {
	home := t.TempDir()
	claudeRoot := filepath.Join(t.TempDir(), "claude-projects")
	t.Setenv("HOME", home)
	t.Setenv("AGENTRUN_CLAUDE_ROOT", claudeRoot)
	t.Setenv("AGENTRUN_CODEX_ROOT", "")

	rt, err := defaultRoots()
	if err != nil {
		t.Fatalf("defaultRoots() error = %v", err)
	}
	if rt.Claude != claudeRoot {
		t.Fatalf("Claude root = %q, want %q", rt.Claude, claudeRoot)
	}
	wantCodex := filepath.Join(home, ".codex", "sessions")
	if rt.Codex != wantCodex {
		t.Fatalf("Codex root = %q, want %q", rt.Codex, wantCodex)
	}
}

func TestRunPathsPrintsRootsWithoutDatabaseURL(t *testing.T) {
	claudeRoot := filepath.Join(t.TempDir(), "claude-projects")
	codexRoot := filepath.Join(t.TempDir(), "codex-sessions")
	t.Setenv("AGENTRUN_CONFIG", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("DATABASE_URL", "")
	t.Setenv("AGENTRUN_CLAUDE_ROOT", claudeRoot)
	t.Setenv("AGENTRUN_CODEX_ROOT", codexRoot)

	out, err := captureStdout(t, func() error {
		return run([]string{"paths"})
	})
	if err != nil {
		t.Fatalf("run(paths) error = %v", err)
	}

	want := "Claude: " + claudeRoot + "\nCodex: " + codexRoot + "\n"
	if out != want {
		t.Fatalf("run(paths) output = %q, want %q", out, want)
	}
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stdout: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = old
	}()

	runErr := fn()
	os.Stdout = old
	if err := w.Close(); err != nil {
		t.Fatalf("close stdout pipe writer: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close stdout pipe reader: %v", err)
	}
	return string(out), runErr
}
