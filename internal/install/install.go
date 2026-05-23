// Package install writes and removes agentrun's hook entries from the user's
// global Claude Code and Codex configuration files. Once installed, every
// claude/codex invocation on the laptop records events into ~/.agentrun/agentrun.db.
//
// Conventions:
//   - Backups: existing files are copied to "<path>.bak.<unix>" before any write.
//   - Idempotency: install removes any previous agentrun entries before adding new ones.
//   - Identification: we recognise our entries by the substring "agentrun hook claude"
//     or "agentrun hook codex" in the command field (Claude) or the AGENTRUN_BEGIN/END
//     marker block (Codex).
package install

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jeevan/agentrun/internal/hooks"
)

// Targets describes the install/uninstall outcome for one config file.
type TargetResult struct {
	Path    string // absolute path of the file we touched
	Action  string // "created" | "updated" | "skipped" | "removed"
	Backup  string // path to the .bak.<ts> file, if one was made
	Message string // optional human-readable note
}

// Result is the aggregate of all targets touched during a single install/uninstall.
type Result struct {
	Claude TargetResult
	Codex  TargetResult
}

// ClaudeSettingsPath returns the absolute path of the user's Claude settings file
// (~/.claude/settings.json).
func ClaudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("UserHomeDir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// CodexConfigPath returns the absolute path of the user's Codex config file
// (~/.codex/config.toml).
func CodexConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("UserHomeDir: %w", err)
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

const (
	codexMarkerBegin = "# === AGENTRUN HOOKS BEGIN — managed by agentrun; do not edit between these markers ==="
	codexMarkerEnd   = "# === AGENTRUN HOOKS END ==="
)

// Install adds agentrun's hooks to ~/.claude/settings.json and ~/.codex/config.toml.
// Existing user hooks are preserved. Re-running Install is safe — prior agentrun
// entries are removed before the fresh ones are added.
func Install(agentrunBin string) (Result, error) {
	var res Result

	claudeRes, err := installClaude(agentrunBin)
	if err != nil {
		return res, fmt.Errorf("install claude: %w", err)
	}
	res.Claude = claudeRes

	codexRes, err := installCodex(agentrunBin)
	if err != nil {
		return res, fmt.Errorf("install codex: %w", err)
	}
	res.Codex = codexRes

	return res, nil
}

// Uninstall removes agentrun's hooks from ~/.claude/settings.json and
// ~/.codex/config.toml. User-authored hooks are left in place.
func Uninstall() (Result, error) {
	var res Result

	claudeRes, err := uninstallClaude()
	if err != nil {
		return res, fmt.Errorf("uninstall claude: %w", err)
	}
	res.Claude = claudeRes

	codexRes, err := uninstallCodex()
	if err != nil {
		return res, fmt.Errorf("uninstall codex: %w", err)
	}
	res.Codex = codexRes

	return res, nil
}

// installClaude rewrites ~/.claude/settings.json so its top-level "hooks"
// object contains agentrun entries for every Claude MVP event. Any pre-existing
// agentrun entries (identifiable by their command string) are replaced. Other
// hook entries the user authored are preserved.
func installClaude(agentrunBin string) (TargetResult, error) {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return TargetResult{}, err
	}

	root, existed, err := readJSONFile(path)
	if err != nil {
		return TargetResult{}, err
	}

	hooksObj, _ := getOrCreateMap(root, "hooks")
	stripAgentrunHooks(hooksObj, "agentrun hook claude ")
	mergeClaudeHooks(hooksObj, hooks.BuildClaudeHooksMap(agentrunBin))
	root["hooks"] = hooksObj

	backup, err := writeJSONFile(path, root, existed)
	if err != nil {
		return TargetResult{}, err
	}

	action := "updated"
	if !existed {
		action = "created"
	}
	return TargetResult{Path: path, Action: action, Backup: backup}, nil
}

// uninstallClaude removes our entries from ~/.claude/settings.json. If the file
// doesn't exist, returns a "skipped" result. User-authored hooks are kept.
func uninstallClaude() (TargetResult, error) {
	path, err := ClaudeSettingsPath()
	if err != nil {
		return TargetResult{}, err
	}

	root, existed, err := readJSONFile(path)
	if err != nil {
		return TargetResult{}, err
	}
	if !existed {
		return TargetResult{Path: path, Action: "skipped", Message: "file does not exist"}, nil
	}

	hooksObj, ok := getOrCreateMap(root, "hooks")
	if !ok {
		// hooks key wasn't a map — leave the file alone but report it.
		return TargetResult{Path: path, Action: "skipped", Message: "no hooks object"}, nil
	}
	stripAgentrunHooks(hooksObj, "agentrun hook claude ")
	if len(hooksObj) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooksObj
	}

	backup, err := writeJSONFile(path, root, true)
	if err != nil {
		return TargetResult{}, err
	}
	return TargetResult{Path: path, Action: "removed", Backup: backup}, nil
}

// installCodex appends agentrun's hook block to ~/.codex/config.toml between
// well-known marker comments. If markers already exist, their content is
// replaced. Other lines of the user's config are preserved verbatim.
func installCodex(agentrunBin string) (TargetResult, error) {
	path, err := CodexConfigPath()
	if err != nil {
		return TargetResult{}, err
	}

	existing, existed, err := readTextFile(path)
	if err != nil {
		return TargetResult{}, err
	}

	cleaned := stripMarkerBlock(existing)
	block := codexMarkerBegin + "\n" +
		"# Records every codex invocation into the agentrun DB.\n" +
		"# Run `agentrun uninstall` to remove these entries.\n\n" +
		hooks.BuildCodexHooksTOML(agentrunBin) +
		codexMarkerEnd + "\n"

	var out string
	if strings.TrimSpace(cleaned) == "" {
		out = block
	} else {
		// Ensure exactly one blank line between the user's content and our block.
		trimmed := strings.TrimRight(cleaned, "\n") + "\n\n"
		out = trimmed + block
	}

	backup, err := writeTextFile(path, out, existed)
	if err != nil {
		return TargetResult{}, err
	}

	action := "updated"
	if !existed {
		action = "created"
	}
	return TargetResult{Path: path, Action: action, Backup: backup}, nil
}

// uninstallCodex removes the marker block from ~/.codex/config.toml.
func uninstallCodex() (TargetResult, error) {
	path, err := CodexConfigPath()
	if err != nil {
		return TargetResult{}, err
	}

	existing, existed, err := readTextFile(path)
	if err != nil {
		return TargetResult{}, err
	}
	if !existed {
		return TargetResult{Path: path, Action: "skipped", Message: "file does not exist"}, nil
	}

	cleaned := stripMarkerBlock(existing)
	if cleaned == existing {
		return TargetResult{Path: path, Action: "skipped", Message: "no agentrun markers present"}, nil
	}

	backup, err := writeTextFile(path, cleaned, true)
	if err != nil {
		return TargetResult{}, err
	}
	return TargetResult{Path: path, Action: "removed", Backup: backup}, nil
}

// readJSONFile reads path as JSON into a generic map. If the file doesn't
// exist, returns an empty map with existed=false (callers will create it).
// If the file exists but is empty or whitespace-only, returns an empty map
// with existed=true.
func readJSONFile(path string) (map[string]any, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, false, nil
		}
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	if strings.TrimSpace(string(b)) == "" {
		return map[string]any{}, true, nil
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w (file is not valid JSON; refusing to overwrite — fix manually or move it aside)", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, true, nil
}

// writeJSONFile writes root as pretty-printed JSON to path. If preBackup is true
// and the file currently exists, it is copied to <path>.bak.<unix-ts> first.
// Returns the backup path (or "" if none).
func writeJSONFile(path string, root map[string]any, preBackup bool) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent of %s: %w", path, err)
	}
	var backup string
	if preBackup {
		if _, err := os.Stat(path); err == nil {
			b, err := backupFile(path)
			if err != nil {
				return "", err
			}
			backup = b
		}
	}
	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return backup, fmt.Errorf("marshal: %w", err)
	}
	if err := atomicWrite(path, append(b, '\n'), 0o644); err != nil {
		return backup, err
	}
	return backup, nil
}

// readTextFile mirrors readJSONFile for plain text. existed=false means the
// file did not exist; content is "".
func readTextFile(path string) (string, bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), true, nil
}

// writeTextFile writes the given content to path, with optional backup.
func writeTextFile(path, content string, preBackup bool) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("mkdir parent of %s: %w", path, err)
	}
	var backup string
	if preBackup {
		if _, err := os.Stat(path); err == nil {
			b, err := backupFile(path)
			if err != nil {
				return "", err
			}
			backup = b
		}
	}
	if err := atomicWrite(path, []byte(content), 0o644); err != nil {
		return backup, err
	}
	return backup, nil
}

// backupFile copies src to "<src>.bak.<unix-ts>". Returns the backup path.
func backupFile(src string) (string, error) {
	dst := fmt.Sprintf("%s.bak.%d", src, time.Now().Unix())
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return "", fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return dst, nil
}

// atomicWrite writes content to a sibling temp file then renames it over path,
// so a crash mid-write doesn't leave a partial file.
func atomicWrite(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename temp -> %s: %w", path, err)
	}
	return nil
}

// getOrCreateMap returns root[key] as map[string]any, creating it if missing.
// Returns ok=false if root[key] exists but is not a map (caller should bail).
func getOrCreateMap(root map[string]any, key string) (map[string]any, bool) {
	if v, present := root[key]; present {
		if m, ok := v.(map[string]any); ok {
			return m, true
		}
		return nil, false
	}
	m := map[string]any{}
	root[key] = m
	return m, true
}

// stripAgentrunHooks walks hooksObj (top-level "hooks" map: event_name -> []group)
// and removes any hook command whose command string contains marker. If a group
// ends up empty, it is dropped. If an event's list ends up empty, the key is removed.
func stripAgentrunHooks(hooksObj map[string]any, marker string) {
	for ev, raw := range hooksObj {
		groups, ok := raw.([]any)
		if !ok {
			continue
		}
		newGroups := groups[:0:0]
		for _, g := range groups {
			grp, ok := g.(map[string]any)
			if !ok {
				newGroups = append(newGroups, g)
				continue
			}
			cmdsRaw, ok := grp["hooks"].([]any)
			if !ok {
				newGroups = append(newGroups, g)
				continue
			}
			kept := cmdsRaw[:0:0]
			for _, c := range cmdsRaw {
				cmd, ok := c.(map[string]any)
				if !ok {
					kept = append(kept, c)
					continue
				}
				cmdStr, _ := cmd["command"].(string)
				if !strings.Contains(cmdStr, marker) {
					kept = append(kept, c)
				}
			}
			if len(kept) > 0 {
				grp["hooks"] = kept
				newGroups = append(newGroups, grp)
			}
		}
		if len(newGroups) == 0 {
			delete(hooksObj, ev)
		} else {
			hooksObj[ev] = newGroups
		}
	}
}

// mergeClaudeHooks appends our generated hookGroup entries to hooksObj, leaving
// any user-authored entries (already stripped of prior agentrun additions) in place.
func mergeClaudeHooks(hooksObj map[string]any, ours map[string][]hooks.HookGroup) {
	for ev, groups := range ours {
		// Encode our typed groups through JSON so they round-trip as generic
		// map[string]any structures matching the rest of the parsed file.
		var anyGroups []any
		b, _ := json.Marshal(groups)
		_ = json.Unmarshal(b, &anyGroups)

		if existing, ok := hooksObj[ev].([]any); ok {
			hooksObj[ev] = append(existing, anyGroups...)
		} else {
			hooksObj[ev] = anyGroups
		}
	}
}

// stripMarkerBlock removes everything between codexMarkerBegin and
// codexMarkerEnd (inclusive of the marker lines). If markers are missing or
// malformed, returns the input unchanged.
func stripMarkerBlock(s string) string {
	beginIdx := strings.Index(s, codexMarkerBegin)
	if beginIdx < 0 {
		return s
	}
	endIdx := strings.Index(s[beginIdx:], codexMarkerEnd)
	if endIdx < 0 {
		return s
	}
	endIdx += beginIdx + len(codexMarkerEnd)
	// Also swallow one trailing newline if present.
	if endIdx < len(s) && s[endIdx] == '\n' {
		endIdx++
	}
	// Trim trailing blank line we may have left.
	left := strings.TrimRight(s[:beginIdx], "\n")
	right := s[endIdx:]
	if left == "" {
		return strings.TrimLeft(right, "\n")
	}
	if right == "" {
		return left + "\n"
	}
	return left + "\n" + strings.TrimLeft(right, "\n")
}
