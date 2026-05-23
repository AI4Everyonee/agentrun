package agent

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestResolveNotFound verifies that looking up a nonexistent binary returns
// an error that wraps ErrNotFound.
func TestResolveNotFound(t *testing.T) {
	_, err := Resolve("nonexistent_xyz_agentrun_test")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
}

// TestResolveSh verifies that "sh" resolves to an absolute path that exists.
func TestResolveSh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell not available on Windows")
	}

	path, err := Resolve("sh")
	if err != nil {
		t.Fatalf("Resolve(\"sh\") returned error: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("expected absolute path, got %q", path)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("os.Stat(%q) returned error: %v", path, statErr)
	}
}

// TestVersionFakeScript verifies that Version returns the trimmed stdout of a
// script that prints "v1.2.3".
func TestVersionFakeScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell scripts not available on Windows")
	}

	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-agent")

	content := "#!/bin/sh\necho \"v1.2.3\"\n"
	if err := os.WriteFile(scriptPath, []byte(content), 0o755); err != nil {
		t.Fatalf("failed to write fake script: %v", err)
	}

	got := Version(scriptPath)
	if got != "v1.2.3" {
		t.Errorf("Version() = %q, want %q", got, "v1.2.3")
	}
}

// TestVersionFalse verifies that Version returns "" when the binary exits non-zero.
func TestVersionFalse(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX /bin/false not available on Windows")
	}

	got := Version("/bin/false")
	if got != "" {
		t.Errorf("Version(\"/bin/false\") = %q, want %q", got, "")
	}
}

// TestVersionNonexistent verifies that Version returns "" (no panic) when the
// binary path does not exist.
func TestVersionNonexistent(t *testing.T) {
	got := Version("/nonexistent/binary/path")
	if got != "" {
		t.Errorf("Version(\"/nonexistent/binary/path\") = %q, want %q", got, "")
	}
}
