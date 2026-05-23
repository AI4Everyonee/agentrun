package agent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ErrNotFound is returned (wrapped) when the agent binary is not on PATH.
var ErrNotFound = errors.New("not found on PATH")

// Resolve looks up the agent binary on PATH. agentName is "claude" or "codex".
// Returns the absolute path. On lookup failure wraps ErrNotFound so callers can
// use errors.Is(err, agent.ErrNotFound).
func Resolve(agentName string) (string, error) {
	path, err := exec.LookPath(agentName)
	if err != nil {
		return "", fmt.Errorf("agent %q: %w", agentName, ErrNotFound)
	}
	return path, nil
}

// Version invokes "<binaryPath> --version" with a 2-second context timeout and
// returns the trimmed stdout. Returns "" on any failure; never an error.
// Version is best-effort metadata only.
func Version(binaryPath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binaryPath, "--version")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
