package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/agent"
	"github.com/AI4Everyonee/agentrun/internal/config"
	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/install"
)

// checkStatus summarises the result of one doctor check.
type checkStatus int

const (
	checkOK   checkStatus = iota // ✓
	checkWarn                    // ⚠
	checkFail                    // ✗
	checkInfo                    // ℹ
)

type checkResult struct {
	status  checkStatus
	message string
}

func pass(msg string) checkResult { return checkResult{checkOK, msg} }
func warn(msg string) checkResult { return checkResult{checkWarn, msg} }
func fail(msg string) checkResult { return checkResult{checkFail, msg} }
func info(msg string) checkResult { return checkResult{checkInfo, msg} }

func (r checkResult) String() string {
	var sym string
	switch r.status {
	case checkOK:
		sym = "✓"
	case checkWarn:
		sym = "⚠"
	case checkFail:
		sym = "✗"
	case checkInfo:
		sym = "ℹ"
	}
	return sym + " " + r.message
}

// runDoctor implements `agentrun doctor`.
// Runs a series of health checks and prints a summary. Exits 1 if any check fails.
func runDoctor(args []string) error {
	if len(args) > 0 {
		return ErrUsage
	}

	fmt.Println("agentrun doctor")
	fmt.Println("==============")

	var results []checkResult

	// 1. claude on PATH
	results = append(results, checkClaudeBinary())

	// 2. codex on PATH
	results = append(results, checkCodexBinary())

	// 3. Claude hooks installed
	results = append(results, checkClaudeHooks())

	// 4. Codex hooks installed
	results = append(results, checkCodexHooks())

	// 5. DB exists + writable / schema version
	cfg, cfgErr := config.Load()
	results = append(results, checkDB(cfg, cfgErr))

	// 6. Recent session activity
	results = append(results, checkRecentSession(cfg, cfgErr))

	// 7. DB size (always info)
	results = append(results, checkDBSize(cfg, cfgErr))

	// Print all results.
	for _, r := range results {
		fmt.Println(r.String())
	}
	fmt.Println()

	// Count failures.
	failures := 0
	for _, r := range results {
		if r.status == checkFail {
			failures++
		}
	}

	if failures == 0 {
		fmt.Println("All checks passed.")
		return nil
	}

	fmt.Printf("%d check(s) failed; see above.\n", failures)
	return fmt.Errorf("%d check(s) failed", failures)
}

func checkClaudeBinary() checkResult {
	path, err := agent.Resolve("claude")
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			return fail("claude not found on PATH")
		}
		return fail("claude: " + err.Error())
	}
	return pass("claude found at " + path)
}

func checkCodexBinary() checkResult {
	path, err := agent.Resolve("codex")
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			return fail("codex not found on PATH")
		}
		return fail("codex: " + err.Error())
	}
	return pass("codex found at " + path)
}

func checkClaudeHooks() checkResult {
	settingsPath, err := install.ClaudeSettingsPath()
	if err != nil {
		return fail("Claude hooks: cannot determine settings path: " + err.Error())
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fail("Claude hooks not installed (settings.json missing)")
		}
		return fail("Claude hooks: cannot read settings.json: " + err.Error())
	}

	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fail("Claude hooks: settings.json is not valid JSON: " + err.Error())
	}

	hooksRaw, ok := root["hooks"]
	if !ok {
		return fail("Claude hooks: no 'hooks' key in settings.json")
	}

	var hooksObj map[string]json.RawMessage
	if err := json.Unmarshal(hooksRaw, &hooksObj); err != nil {
		return fail("Claude hooks: 'hooks' is not an object: " + err.Error())
	}

	// Count hook entries whose command contains "agentrun hook claude".
	count := countAgentrunClaudeHooks(hooksObj)

	switch {
	case count == 0:
		return fail("Claude hooks not installed (0 agentrun hook entries found)")
	case count < 7:
		return warn(fmt.Sprintf("Claude hooks partially installed (%d events; expected ≥7)", count))
	default:
		return pass(fmt.Sprintf("Claude hooks installed (%d events)", count))
	}
}

// countAgentrunClaudeHooks counts hook command entries containing "agentrun hook claude"
// across all event types in the hooks object.
func countAgentrunClaudeHooks(hooksObj map[string]json.RawMessage) int {
	count := 0
	for _, rawGroups := range hooksObj {
		var groups []map[string]json.RawMessage
		if err := json.Unmarshal(rawGroups, &groups); err != nil {
			continue
		}
		for _, group := range groups {
			hooksRaw, ok := group["hooks"]
			if !ok {
				continue
			}
			var hookCmds []map[string]json.RawMessage
			if err := json.Unmarshal(hooksRaw, &hookCmds); err != nil {
				continue
			}
			for _, cmd := range hookCmds {
				cmdRaw, ok := cmd["command"]
				if !ok {
					continue
				}
				var cmdStr string
				if err := json.Unmarshal(cmdRaw, &cmdStr); err != nil {
					continue
				}
				if strings.Contains(cmdStr, "agentrun hook claude") {
					count++
				}
			}
		}
	}
	return count
}

func checkCodexHooks() checkResult {
	configPath, err := install.CodexConfigPath()
	if err != nil {
		return fail("Codex hooks: cannot determine config path: " + err.Error())
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fail("Codex hooks not installed (config.toml missing)")
		}
		return fail("Codex hooks: cannot read config.toml: " + err.Error())
	}

	const marker = "# === AGENTRUN HOOKS BEGIN"
	if !strings.Contains(string(data), marker) {
		return fail("Codex hooks not installed (AGENTRUN HOOKS BEGIN marker missing)")
	}
	return pass("Codex hooks installed")
}

func checkDB(cfg config.Config, cfgErr error) checkResult {
	if cfgErr != nil {
		return fail("DB: config error: " + cfgErr.Error())
	}

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return fail("DB: cannot open " + cfg.DBPath + ": " + err.Error())
	}
	defer d.Close()

	current, err := db.CurrentSchemaVersion(d)
	if err != nil {
		return fail("DB: cannot read schema version: " + err.Error())
	}
	latest := db.LatestSchemaVersion()

	if current != latest {
		return warn(fmt.Sprintf("DB at %s (schema v%d, expected v%d — run agentrun to migrate)",
			cfg.DBPath, current, latest))
	}
	return pass(fmt.Sprintf("DB at %s (schema v%d)", cfg.DBPath, current))
}

func checkRecentSession(cfg config.Config, cfgErr error) checkResult {
	if cfgErr != nil {
		return warn("Recent session: config error: " + cfgErr.Error())
	}

	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return warn("Recent session: cannot open DB: " + err.Error())
	}
	defer d.Close()

	rows, err := db.ListSessions(d, 1)
	if err != nil {
		return warn("Recent session: query failed: " + err.Error())
	}

	if len(rows) == 0 {
		return warn("No sessions recorded yet")
	}

	r := rows[0]
	age := time.Since(r.StartedAt)
	if age > 7*24*time.Hour {
		return warn(fmt.Sprintf("No sessions in the last 7 days (most recent: %s)",
			r.StartedAt.Local().Format("2006-01-02 15:04")))
	}

	status := "?"
	if r.Status.Valid {
		status = r.Status.String
	}
	return pass(fmt.Sprintf("Most recent session: %s (%s, %s)",
		r.StartedAt.Local().Format("2006-01-02 15:04"),
		r.Agent,
		status,
	))
}

func checkDBSize(cfg config.Config, cfgErr error) checkResult {
	if cfgErr != nil {
		return info("DB size: config error: " + cfgErr.Error())
	}

	fi, err := os.Stat(cfg.DBPath)
	if err != nil {
		if os.IsNotExist(err) {
			return info("DB size: file does not exist yet")
		}
		return info("DB size: stat error: " + err.Error())
	}

	return info(fmt.Sprintf("DB size: %s", formatBytes(fi.Size())))
}

// formatBytes converts a byte count to a human-readable string (KB/MB/GB).
func formatBytes(n int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
