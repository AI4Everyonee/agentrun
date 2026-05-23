// Package userident resolves the laptop user's identity so the recorder can
// attribute every session row to a specific person. Intended for cloud-DB
// deployments where one DB pools sessions from many users.
//
// Resolution order (first non-empty wins):
//  1. $AGENTRUN_USER         — explicit override (recommended for cloud setups)
//  2. $USER                  — POSIX shell default
//  3. $LOGNAME               — older POSIX fallback
//  4. $USERPROFILE basename  — Windows-y env
//  5. `git config user.email`/`user.name` — best last resort, identifies
//                                            the developer even in CI sandboxes
//  6. ""                     — give up; sessions.user_name stays NULL
//
// The git probe runs with a 1-second timeout and is silently ignored on
// failure (same pattern as internal/gitmeta).
package userident

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Detect returns the user identity string. Empty string means we couldn't
// resolve one — caller should write NULL into sessions.user_name.
func Detect() string {
	if v := strings.TrimSpace(os.Getenv("AGENTRUN_USER")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("USER")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("LOGNAME")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("USERPROFILE")); v != "" {
		return filepath.Base(v)
	}
	if v := gitConfig("user.email"); v != "" {
		return v
	}
	if v := gitConfig("user.name"); v != "" {
		return v
	}
	return ""
}

// gitConfig runs `git config --get <key>` in the current working directory
// with a 1-second timeout. Empty string on any failure.
func gitConfig(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "config", "--get", key)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
