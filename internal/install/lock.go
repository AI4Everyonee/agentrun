//go:build !windows

package install

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireInstallLock obtains an exclusive advisory lock on
// $HOME/.agentrun/install.lock so concurrent `agentrun install/uninstall`
// invocations don't race when rewriting ~/.claude/settings.json or
// ~/.codex/config.toml.
//
// The returned function releases the lock and closes the file. Always defer it.
//
// On Windows we'd want a different mechanism (LockFileEx); for now the build
// tag excludes that platform — same scoping as internal/pty.
func acquireInstallLock() (func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("acquireInstallLock: UserHomeDir: %w", err)
	}
	lockDir := filepath.Join(home, ".agentrun")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, fmt.Errorf("acquireInstallLock: mkdir %q: %w", lockDir, err)
	}
	lockPath := filepath.Join(lockDir, "install.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("acquireInstallLock: open %q: %w", lockPath, err)
	}
	// Blocking exclusive lock — a parallel install simply waits its turn.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("acquireInstallLock: flock %q: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
