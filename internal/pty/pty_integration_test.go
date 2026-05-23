//go:build !windows

package pty_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/jeevan/agentrun/internal/pty"
)

// TestPTYIntegration spawns /bin/cat under a PTY and verifies that Start
// returns a valid Handle and that Close + Wait behave correctly.
//
// This test requires a real TTY and a usable PTY device; it is gated behind
// the AGENTRUN_PTY_TEST=1 environment variable to avoid flakiness in CI
// environments that run go test without a PTY.
func TestPTYIntegration(t *testing.T) {
	if os.Getenv("AGENTRUN_PTY_TEST") != "1" {
		t.Skip("set AGENTRUN_PTY_TEST=1 to run PTY integration tests")
	}

	cmd := exec.Command("/bin/cat")
	h, err := pty.Start(cmd, nil, nil)
	if err != nil {
		t.Fatalf("Start(/bin/cat) failed: %v", err)
	}
	if h == nil {
		t.Fatal("Start returned nil Handle without error")
	}

	// Close the PTY; /bin/cat should exit when its stdin closes.
	if err := h.Close(); err != nil {
		t.Logf("Close() returned (possibly expected) error: %v", err)
	}

	// Wait must return without hanging and return an integer exit code.
	code := h.Wait()
	t.Logf("Wait() returned exit code %d", code)

	// Close and Wait must be idempotent.
	_ = h.Close()
	code2 := h.Wait()
	if code2 != code {
		t.Errorf("second Wait() returned %d, want %d (idempotency broken)", code2, code)
	}
}
