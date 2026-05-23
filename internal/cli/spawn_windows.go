//go:build windows

package cli

import "os/exec"

// spawnDetached: Windows fallback. We just Start the child without Wait.
// True detachment requires DETACHED_PROCESS in CreateProcess flags; for now
// the child shares the console, which may keep the parent alive longer than
// desired. Acceptable until someone with a Windows setup needs it.
func spawnDetached(binary string, args ...string) error {
	cmd := exec.Command(binary, args...)
	return cmd.Start()
}
