package hooks

import (
	"fmt"
	"strings"
)

// codexMVPEvents is the subset of MVPEvents that Codex actually emits per its
// documented lifecycle-hook surface. Codex does NOT support PostToolUseFailure,
// PermissionDenied, Notification, or SessionEnd as of writing; those Claude-only
// events are silently omitted when registering against Codex.
//
// Stable order — tests compare against it.
var codexMVPEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"PreToolUse",
	"PostToolUse",
	"PermissionRequest",
	"Stop",
	"PreCompact",
}

// CodexMVPEvents returns the event names we register against Codex sessions.
func CodexMVPEvents() []string {
	out := make([]string, len(codexMVPEvents))
	copy(out, codexMVPEvents)
	return out
}

// BuildCodexHooksTOML returns the TOML block that registers agentrun's hook
// subcommand for every Codex MVP event. Intended for appending to
// ~/.codex/config.toml between AGENTRUN markers (see the install package).
//
// Codex uses the same hook payload schema as Claude — the only differences
// from Claude's hooks are the config file format (TOML vs JSON) and the
// argv form embeds "codex" so the hook command knows which agent invoked it.
func BuildCodexHooksTOML(agentrunBinary string) string {
	var b strings.Builder
	quotedBin := tomlEscapeBasicString(agentrunBinary)
	for _, ev := range codexMVPEvents {
		// Codex accepts the same array-of-tables structure as Claude's hooks.json.
		fmt.Fprintf(&b, "[[hooks.%s]]\n", ev)
		fmt.Fprintf(&b, "[[hooks.%s.hooks]]\n", ev)
		fmt.Fprintf(&b, "type = \"command\"\n")
		fmt.Fprintf(&b, "command = \"%s hook codex %s\"\n", quotedBin, ev)
		fmt.Fprintf(&b, "timeout = 10\n\n")
	}
	return b.String()
}

// tomlEscapeBasicString escapes a string for inclusion inside a TOML
// double-quoted ("basic") string. We escape backslash and double-quote;
// other control chars are not expected in absolute paths.
func tomlEscapeBasicString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}
