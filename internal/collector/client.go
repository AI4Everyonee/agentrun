package collector

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// socketTimeout is the maximum time we'll wait for the collector to respond.
// Kept intentionally short: if the collector is unavailable or hung, the hook
// must fall back to direct DB write quickly so the parent agent isn't delayed.
const socketTimeout = 50 * time.Millisecond

// DefaultSocketPath returns the default collector socket path: <DBDir>/collector.sock.
// Uses the same env-resolution logic as resolveDBPath in internal/cli/hook.go.
func DefaultSocketPath() string {
	if p := os.Getenv("AGENTRUN_COLLECTOR_SOCK"); p != "" {
		return p
	}
	return filepath.Join(dbDir(), "collector.sock")
}

// DefaultPIDPath returns the default PID file path: <DBDir>/collector.pid.
func DefaultPIDPath() string {
	return filepath.Join(dbDir(), "collector.pid")
}

// dbDir resolves the agentrun data directory from environment, matching the
// logic in config.Load and cli/hook.go resolveDBPath.
func dbDir() string {
	if dir := os.Getenv("AGENTRUN_DB_DIR"); dir != "" {
		abs, err := filepath.Abs(dir)
		if err == nil {
			return abs
		}
	}
	if p := os.Getenv("AGENTRUN_DB_PATH"); p != "" {
		return filepath.Dir(p)
	}
	home, err := os.UserHomeDir()
	if err == nil && home != "" {
		return filepath.Join(home, ".agentrun")
	}
	return filepath.Join(os.TempDir(), "agentrun")
}

// TrySend attempts to deliver one hook event to a running collector server.
// It connects to socketPath with a 50ms deadline, writes the JSON request,
// and reads a one-byte response code.
//
// Returns nil if the collector accepted the event (ResponseOK).
// Returns a non-nil error on any failure — no socket, timeout, error byte —
// so the caller can fall back to a direct DB write without delay.
func TrySend(socketPath string, req Request) error {
	conn, err := net.DialTimeout("unix", socketPath, socketTimeout)
	if err != nil {
		return fmt.Errorf("collector: dial %s: %w", socketPath, err)
	}
	defer conn.Close()

	// Apply an end-to-end deadline covering both the write and the read.
	if err := conn.SetDeadline(time.Now().Add(socketTimeout)); err != nil {
		return fmt.Errorf("collector: set deadline: %w", err)
	}

	line, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("collector: marshal request: %w", err)
	}
	line = append(line, '\n')

	if _, err := conn.Write(line); err != nil {
		return fmt.Errorf("collector: write request: %w", err)
	}

	// Read response: one byte + newline.
	resp := make([]byte, 2)
	if _, err := readFull(conn, resp); err != nil {
		return fmt.Errorf("collector: read response: %w", err)
	}
	if resp[0] == ResponseErr {
		return fmt.Errorf("collector: server returned error")
	}
	if resp[0] != ResponseOK {
		return fmt.Errorf("collector: unexpected response byte 0x%02x", resp[0])
	}
	return nil
}

// readFull reads exactly len(buf) bytes from conn.
func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
