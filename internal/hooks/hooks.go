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

// HookCommand is the command entry inside a hook group.
type HookCommand struct {
	Type    string `json:"type"`              // always "command"
	Command string `json:"command"`            // "<agentrun> hook <agent> <Event>"
	Timeout int    `json:"timeout,omitempty"`  // seconds; 10 for all events
}

// HookGroup is one entry in the per-event hook list.
type HookGroup struct {
	Matcher *string       `json:"matcher,omitempty"` // nil => omitted; &"" => emitted
	Hooks   []HookCommand `json:"hooks"`
}

// settingsFile is the top-level structure written to hooks.json.
type settingsFile struct {
	Hooks map[string][]HookGroup `json:"hooks"`
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

// BuildClaudeHooksJSON returns the JSON bytes for a Claude Code settings file
// that registers every MVPEvents() entry against "<agentrunBinary> hook claude <EventName>".
//
// The shape is the same structure Claude consumes from ~/.claude/settings.json's
// top-level "hooks" key — callers can either write the bytes verbatim to a file
// (when using --settings <file>) or merge the parsed map into an existing
// settings file (the install path).
func BuildClaudeHooksJSON(agentrunBinary string) ([]byte, error) {
	out := settingsFile{Hooks: BuildClaudeHooksMap(agentrunBinary)}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("hooks.BuildClaudeHooksJSON: marshal: %w", err)
	}
	return b, nil
}

// BuildClaudeHooksMap returns the hooks-by-event-name map that callers can
// merge into an existing settings.json file. Each command is shell-quoted.
//
// Marker convention: every command string starts with the agentrun binary path
// followed by " hook claude " — install/uninstall use this substring to find
// and remove our entries.
func BuildClaudeHooksMap(agentrunBinary string) map[string][]HookGroup {
	out := map[string][]HookGroup{}
	empty := ""
	for _, ev := range MVPEvents() {
		var matcher *string
		switch ev {
		case "UserPromptSubmit", "Stop":
			matcher = nil
		default:
			matcher = &empty
		}
		cmd := fmt.Sprintf("%s hook claude %s", shellQuote(agentrunBinary), ev)
		out[ev] = []HookGroup{
			{
				Matcher: matcher,
				Hooks:   []HookCommand{{Type: "command", Command: cmd, Timeout: 10}},
			},
		}
	}
	return out
}

// GenerateSettings writes the per-session settings JSON to <SessionDir>/hooks.json.
// Used by the wrapper's opt-in PTY+hooks path; the global install flow uses
// BuildClaudeHooksJSON/Map directly to merge into ~/.claude/settings.json.
func GenerateSettings(dbDir, sessionID, agentrunBinary string) (string, error) {
	dir := SessionDir(dbDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("hooks.GenerateSettings: mkdir %q: %w", dir, err)
	}
	b, err := BuildClaudeHooksJSON(agentrunBinary)
	if err != nil {
		return "", err
	}
	path := SettingsPath(dbDir, sessionID)
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
