// Package config resolves the agentrun runtime configuration from environment
// variables and the current working directory.
//
// Only AGENTRUN_DB_DIR is honored in Phase 1. A future
// ~/.config/agentrun/config.yaml is Phase 6+.
package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Config is the resolved runtime configuration.
type Config struct {
	DBDir        string // absolute path
	DBPath       string // <DBDir>/agentrun.db
	ArtifactsDir string // <DBDir>/artifacts
}

// Load resolves config from env + cwd. It does NOT create directories;
// callers (recorder.Start, db.Open) create them lazily.
//
// Resolution order for DBDir:
//  1. If env AGENTRUN_DB_DIR is non-empty, use it (run through filepath.Abs).
//  2. Else, shell out to "git rev-parse --show-toplevel" in cwd with a 1s
//     timeout. On success, use <repo_root>/.agentrun.
//  3. Else, use <$HOME>/.agentrun. If HOME is unset or empty, fall back to
//     <os.TempDir()>/agentrun and print a warning to stderr.
func Load() (Config, error) {
	dbDir, err := resolveDBDir()
	if err != nil {
		return Config{}, err
	}

	return Config{
		DBDir:        dbDir,
		DBPath:       filepath.Join(dbDir, "agentrun.db"),
		ArtifactsDir: filepath.Join(dbDir, "artifacts"),
	}, nil
}

// resolveDBDir determines the absolute path for the agentrun data directory.
func resolveDBDir() (string, error) {
	// 1. Explicit env var wins.
	if envDir := os.Getenv("AGENTRUN_DB_DIR"); envDir != "" {
		abs, err := filepath.Abs(envDir)
		if err != nil {
			return "", fmt.Errorf("config: cannot absolutize AGENTRUN_DB_DIR %q: %w", envDir, err)
		}
		return abs, nil
	}

	// 2. Try git rev-parse --show-toplevel in cwd.
	if repoRoot := gitRepoRoot(); repoRoot != "" {
		return filepath.Join(repoRoot, ".agentrun"), nil
	}

	// 3. Fall back to $HOME/.agentrun (or os.TempDir()/agentrun).
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fallback := filepath.Join(os.TempDir(), "agentrun")
		fmt.Fprintf(os.Stderr, "agentrun: warning: cannot determine home directory (%v), using %s\n", err, fallback)
		return fallback, nil
	}
	return filepath.Join(home, ".agentrun"), nil
}

// gitRepoRoot shells out to git rev-parse --show-toplevel in the process cwd
// with a 1-second timeout. Returns the trimmed output on success, or "" on any
// failure (git not on PATH, not in a repo, timeout, etc.).
func gitRepoRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	return strings.TrimSpace(string(out))
}
