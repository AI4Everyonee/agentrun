//go:build windows

package install

// Windows fallback: no advisory lock. Concurrent installs on Windows will race;
// document this as a known limitation. A future change can use LockFileEx.
func acquireInstallLock() (func(), error) {
	return func() {}, nil
}
