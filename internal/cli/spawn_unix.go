//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// spawnDetached starts a child that survives the parent's exit. The child
// becomes its own session leader (Setsid) and has its stdio attached to
// /dev/null so it doesn't write back into the user's terminal. We do NOT
// call cmd.Wait — the caller wants to return immediately.
func spawnDetached(binary string, args ...string) error {
	cmd := exec.Command(binary, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Start()
}
