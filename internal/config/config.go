// Package config resolves the agentrun runtime configuration from environment
// variables.
//
// In the global-recorder model, the DB lives in a single fixed location under
// the user's home directory (~/.agentrun) so that sessions from any cwd land
// in one place. Override via AGENTRUN_DB_DIR for tests or per-user variants.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// Config is the resolved runtime configuration.
type Config struct {
	DBDir        string // absolute path
	DBPath       string // <DBDir>/agentrun.db
	ArtifactsDir string // <DBDir>/artifacts
}

// Load resolves config from env. It does NOT create directories; callers
// (recorder.Start, db.Open) create them lazily.
//
// Resolution order for DBDir:
//  1. If env AGENTRUN_DB_DIR is non-empty, use it (run through filepath.Abs).
//  2. Else, use <$HOME>/.agentrun.
//  3. If HOME is unset or empty, fall back to <os.TempDir()>/agentrun and
//     print a one-line warning to stderr.
//
// Note: prior versions of agentrun preferred the git repo root for DBDir.
// That was removed when the recorder became a global hook receiver — sessions
// fire from anywhere on the laptop and must land in one shared DB.
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
	if envDir := os.Getenv("AGENTRUN_DB_DIR"); envDir != "" {
		abs, err := filepath.Abs(envDir)
		if err != nil {
			return "", fmt.Errorf("config: cannot absolutize AGENTRUN_DB_DIR %q: %w", envDir, err)
		}
		return abs, nil
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		fallback := filepath.Join(os.TempDir(), "agentrun")
		fmt.Fprintf(os.Stderr, "agentrun: warning: cannot determine home directory (%v), using %s\n", err, fallback)
		return fallback, nil
	}
	return filepath.Join(home, ".agentrun"), nil
}
