//go:build !windows

package pty_test

import (
	"os/exec"
	"testing"

	"github.com/AI4Everyonee/agentrun/internal/pty"
)

// TestStartRejectsUnset verifies that Start returns an error when cmd has no
// Path set, without panicking or touching the terminal.
func TestStartRejectsUnset(t *testing.T) {
	cmd := &exec.Cmd{} // no Path set
	_, err := pty.Start(cmd, nil, nil)
	if err == nil {
		t.Fatal("expected error for cmd with no Path")
	}
}

// TestStartRejectsNil verifies that Start returns an error when cmd is nil.
func TestStartRejectsNil(t *testing.T) {
	_, err := pty.Start(nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for nil cmd")
	}
}
