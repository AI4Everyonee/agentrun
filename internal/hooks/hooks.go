package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// safePathRE matches paths that need no shell quoting.
var safePathRE = regexp.MustCompile(`^[A-Za-z0-9_/.@:+%~,=-]+$`)

// hookCommand is the command entry inside a hook group.
type hookCommand struct {
	Type    string `json:"type"`              // always "command"
	Command string `json:"command"`            // "<agentrun> hook <Event>"
	Timeout int    `json:"timeout,omitempty"`  // seconds; 10 for all events
}

// hookGroup is one entry in the per-event hook list.
type hookGroup struct {
	Matcher *string       `json:"matcher,omitempty"` // nil => omitted; &"" => emitted
	Hooks   []hookCommand `json:"hooks"`
}

// settingsFile is the top-level structure written to hooks.json.
type settingsFile struct {
	Hooks map[string][]hookGroup `json:"hooks"`
}

// SessionDir returns the absolute directory path where per-session derived
// state lives. Layout: <dbDir>/sessions/<sessionID>/
func SessionDir(dbDir, sessionID string) string {
	return filepath.Join(dbDir, "sessions", sessionID)
}

// SettingsPath returns <SessionDir>/hooks.json.
func SettingsPath(dbDir, sessionID string) string {
	return filepath.Join(SessionDir(dbDir, sessionID), "hooks.json")
}

// GenerateSettings writes the per-session settings JSON to <SessionDir>/hooks.json.
// The file contains hook registrations for every MVPEvents() entry, each pointing
// to "<agentrunBinary> hook <EventName>".
//
//   dbDir          — agentrun's data directory (used to derive the session dir)
//   sessionID      — our s_<ulid>
//   agentrunBinary — absolute path to the running agentrun binary (from os.Executable)
//
// Returns the absolute path written. The directory is created with 0o755; the
// file is written with 0o644.
func GenerateSettings(dbDir, sessionID, agentrunBinary string) (string, error) {
	dir := SessionDir(dbDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("hooks.GenerateSettings: mkdir %q: %w", dir, err)
	}

	out := settingsFile{Hooks: map[string][]hookGroup{}}
	empty := ""

	for _, ev := range MVPEvents() {
		var matcher *string
		switch ev {
		case "UserPromptSubmit", "Stop":
			matcher = nil
		default:
			matcher = &empty
		}
		cmd := fmt.Sprintf("%s hook %s", shellQuote(agentrunBinary), ev)
		out.Hooks[ev] = []hookGroup{
			{
				Matcher: matcher,
				Hooks:   []hookCommand{{Type: "command", Command: cmd, Timeout: 10}},
			},
		}
	}

	path := SettingsPath(dbDir, sessionID)
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return "", fmt.Errorf("hooks.GenerateSettings: marshal: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", fmt.Errorf("hooks.GenerateSettings: write %q: %w", path, err)
	}
	return path, nil
}

// Cleanup removes <SessionDir(dbDir, sessionID)>. Best-effort; errors are
// returned but the caller is expected to log-and-continue.
func Cleanup(dbDir, sessionID string) error {
	if err := os.RemoveAll(SessionDir(dbDir, sessionID)); err != nil {
		return fmt.Errorf("hooks.Cleanup: %w", err)
	}
	return nil
}

// shellQuote returns p quoted for inclusion in a sh -c command.
// If p consists only of safe chars [A-Za-z0-9_/.@:+%~,=-], returns p unchanged.
// Otherwise wraps in single quotes; any embedded ' becomes '\''.
func shellQuote(p string) string {
	if safePathRE.MatchString(p) {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
