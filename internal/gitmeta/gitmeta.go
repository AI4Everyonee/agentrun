// Package gitmeta provides cheap, best-effort git metadata capture at session
// boundaries. All git shell-outs are silently dropped on failure — per
// GOAL.md §9.4, git capture must never fail the whole session when outside a
// git repo or when git is unavailable.
package gitmeta

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Snapshot bundles the cheap git facts we want at session boundaries.
type Snapshot struct {
	RepoRoot string // "" if not in a repo or git unavailable
	Branch   string // "" if detached HEAD or not in a repo
	HeadSHA  string // "" if no commits or not in a repo
}

// Capture runs git in cwd to fill a Snapshot. Each shell-out has a 1s timeout.
// Never returns an error: partial results are fine.
func Capture(cwd string) Snapshot {
	var s Snapshot
	s.RepoRoot = runGit(cwd, "rev-parse", "--show-toplevel")
	branch := runGit(cwd, "rev-parse", "--abbrev-ref", "HEAD")
	// A detached HEAD prints the literal string "HEAD"; treat that as no branch.
	if branch != "HEAD" {
		s.Branch = branch
	}
	s.HeadSHA = runGit(cwd, "rev-parse", "HEAD")
	return s
}

// HeadCommit returns just the HEAD SHA. Empty string on any failure.
func HeadCommit(cwd string) string {
	return runGit(cwd, "rev-parse", "HEAD")
}

// runGit executes git with the given args in cwd with a 1s timeout.
// Returns the trimmed stdout on success, or "" on any error.
func runGit(cwd string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
