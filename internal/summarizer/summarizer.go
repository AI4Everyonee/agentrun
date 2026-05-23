// Package summarizer turns a recorded session into a one-paragraph English
// summary by calling OpenAI's chat-completions endpoint with a small reasoning
// model. The result is intended to feed later skill-synthesis pipelines and
// human inspection — NOT to drive any agent behavior.
//
// The summarizer is fail-soft: any failure (missing API key, network error,
// API rate limit, malformed response) results in a NULL summary column rather
// than blocking session finalization. The recorder always returns success.
//
// Configuration via environment:
//
//	OPENAI_API_KEY            — required; if absent, summarizer is a no-op
//	AGENTRUN_SUMMARY_MODEL    — override the model (default: gpt-5.5-mini)
//	AGENTRUN_SUMMARY_DISABLED — set to "1" to disable summarization for a session
//	AGENTRUN_SUMMARY_TIMEOUT  — Go duration (e.g. "30s") for the HTTP call
package summarizer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// defaultModel is the current shipping reasoning-capable mini.
	// Override via $AGENTRUN_SUMMARY_MODEL — known-good alternatives:
	//   gpt-5-mini, gpt-4.1-mini (cheaper, no reasoning).
	defaultModel    = "gpt-5.4-mini"
	defaultTimeout  = 30 * time.Second
	maxPromptChars  = 6000  // truncate per-prompt text
	maxToolChars    = 2000  // truncate per-tool-input text
	maxSessionChars = 30000 // overall corpus cap before sending
	openAIEndpoint  = "https://api.openai.com/v1/chat/completions"
)

// SessionInput is the digest of a session passed to Summarize. The caller
// shapes this from the events table; we deliberately keep it small (under
// ~30 KiB total) so the model's context spend stays predictable.
type SessionInput struct {
	SessionID   string
	Agent       string // "claude" | "codex"
	Cwd         string
	RepoRoot    string
	StartedAt   time.Time
	EndedAt     time.Time
	Prompts     []string  // user.prompt event payloads, in order
	ToolCalls   []ToolCall // PreToolUse event digests, in order
	FilesChanged []string // unique file paths from filesystem events
	Errors      []string  // tool.failed digests
}

// ToolCall is one ordered tool invocation pulled from PreToolUse events.
type ToolCall struct {
	Name  string // "Bash" | "Edit" | "Read" | ...
	Input string // truncated JSON or one-line digest
}

// Result is the summarizer's output. CostCents is best-effort; 0 means unknown.
type Result struct {
	Summary    string
	Model      string
	Tokens     int
	CostCents  int
}

// Summarize calls OpenAI's chat-completions API to render a one-paragraph
// summary of a session. Returns ("", nil) when the summarizer is disabled or
// no API key is configured. Returns ("", err) on hard API failures.
//
// IMPORTANT: callers must always treat (Result, error) the same way — log the
// error, persist whatever Summary is non-empty, never propagate the error up.
// We never want a summarizer hiccup to mark a session as failed.
func Summarize(ctx context.Context, in SessionInput) (Result, error) {
	if os.Getenv("AGENTRUN_SUMMARY_DISABLED") == "1" {
		return Result{}, nil
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return Result{}, nil
	}

	model := os.Getenv("AGENTRUN_SUMMARY_MODEL")
	if model == "" {
		model = defaultModel
	}

	timeout := defaultTimeout
	if v := os.Getenv("AGENTRUN_SUMMARY_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			timeout = d
		}
	}

	corpus := renderCorpus(in)
	if corpus == "" {
		return Result{}, nil
	}

	body, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": corpus},
		},
		"reasoning_effort": "low",
		"max_completion_tokens": 600,
	})
	if err != nil {
		return Result{}, fmt.Errorf("summarizer: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIEndpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("summarizer: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("summarizer: POST: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Read first 1 KiB of the body for a useful error message.
		buf := make([]byte, 1024)
		n, _ := resp.Body.Read(buf)
		return Result{}, fmt.Errorf("summarizer: HTTP %d: %s", resp.StatusCode, string(buf[:n]))
	}

	var rawResp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawResp); err != nil {
		return Result{}, fmt.Errorf("summarizer: decode: %w", err)
	}
	if len(rawResp.Choices) == 0 {
		return Result{}, fmt.Errorf("summarizer: no choices in response")
	}

	summary := strings.TrimSpace(rawResp.Choices[0].Message.Content)
	out := Result{
		Summary: summary,
		Model:   rawResp.Model,
		Tokens:  rawResp.Usage.TotalTokens,
	}
	if out.Model == "" {
		out.Model = model
	}
	return out, nil
}

const systemPrompt = `You are an audit assistant that writes ONE concise paragraph
(under 80 words) describing what happened in a coding-agent session. Cover:
- the task the user asked for (verb + object)
- the agent's main approach (read files, edited X, ran tests, etc.)
- the outcome (succeeded, failed, blocked, pending)

Be specific: name file paths and commands when known. No fluff, no preamble,
no quotation marks around the summary. Plain text only.`

// renderCorpus produces the user-message body sent to the model. We strip
// excess noise and cap by overall length so we don't blow context budget on
// pathologically long sessions.
func renderCorpus(in SessionInput) string {
	var b strings.Builder
	dur := in.EndedAt.Sub(in.StartedAt)
	fmt.Fprintf(&b, "Agent: %s\n", in.Agent)
	if in.RepoRoot != "" {
		fmt.Fprintf(&b, "Repo: %s\n", in.RepoRoot)
	} else if in.Cwd != "" {
		fmt.Fprintf(&b, "Cwd: %s\n", in.Cwd)
	}
	if dur > 0 {
		fmt.Fprintf(&b, "Duration: %s\n", dur.Round(time.Second))
	}
	b.WriteString("\n")

	if len(in.Prompts) > 0 {
		b.WriteString("User prompts:\n")
		for i, p := range in.Prompts {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, truncate(p, maxPromptChars))
		}
		b.WriteString("\n")
	}

	if len(in.ToolCalls) > 0 {
		b.WriteString("Tool calls (in order):\n")
		for i, t := range in.ToolCalls {
			fmt.Fprintf(&b, "  %d. %s — %s\n", i+1, t.Name, truncate(t.Input, maxToolChars))
		}
		b.WriteString("\n")
	}

	if len(in.FilesChanged) > 0 {
		b.WriteString("Files changed:\n")
		for _, f := range in.FilesChanged {
			fmt.Fprintf(&b, "  - %s\n", f)
		}
		b.WriteString("\n")
	}

	if len(in.Errors) > 0 {
		b.WriteString("Errors:\n")
		for _, e := range in.Errors {
			fmt.Fprintf(&b, "  - %s\n", truncate(e, maxToolChars))
		}
	}

	out := b.String()
	if len(out) > maxSessionChars {
		out = out[:maxSessionChars] + "\n…(truncated for length)"
	}
	return strings.TrimSpace(out)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// BuildInput assembles a SessionInput by reading from the DB. Used by the
// summarize subcommand to avoid re-shipping the data through stdin.
func BuildInput(d *sql.DB, sessionID string) (SessionInput, error) {
	row := d.QueryRow(`
		SELECT agent, cwd, COALESCE(repo_root, ''), started_at, COALESCE(ended_at, started_at)
		  FROM sessions WHERE id = ?`, sessionID)
	var in SessionInput
	in.SessionID = sessionID
	var startedAt, endedAt string
	if err := row.Scan(&in.Agent, &in.Cwd, &in.RepoRoot, &startedAt, &endedAt); err != nil {
		return SessionInput{}, fmt.Errorf("BuildInput: scan session: %w", err)
	}
	if t, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
		in.StartedAt = t
	}
	if t, err := time.Parse(time.RFC3339Nano, endedAt); err == nil {
		in.EndedAt = t
	}

	prompts, err := fetchPromptsForSession(d, sessionID)
	if err != nil {
		return SessionInput{}, err
	}
	in.Prompts = prompts

	tools, err := fetchToolCallsForSession(d, sessionID)
	if err != nil {
		return SessionInput{}, err
	}
	in.ToolCalls = tools

	files, err := fetchFilesChanged(d, sessionID)
	if err != nil {
		return SessionInput{}, err
	}
	in.FilesChanged = files

	errs, err := fetchToolErrors(d, sessionID)
	if err != nil {
		return SessionInput{}, err
	}
	in.Errors = errs

	return in, nil
}

func fetchPromptsForSession(d *sql.DB, sessionID string) ([]string, error) {
	rows, err := d.Query(`SELECT payload_json FROM events WHERE session_id = ? AND type = 'user.prompt' ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("fetchPrompts: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			continue
		}
		var probe struct {
			Prompt string `json:"prompt"`
		}
		if err := json.Unmarshal(p, &probe); err != nil || probe.Prompt == "" {
			continue
		}
		out = append(out, probe.Prompt)
	}
	return out, rows.Err()
}

func fetchToolCallsForSession(d *sql.DB, sessionID string) ([]ToolCall, error) {
	rows, err := d.Query(`SELECT payload_json FROM events WHERE session_id = ? AND type = 'tool.pre_use' ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("fetchToolCalls: %w", err)
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			continue
		}
		var probe struct {
			ToolName  string         `json:"tool_name"`
			ToolInput map[string]any `json:"tool_input"`
		}
		if err := json.Unmarshal(p, &probe); err != nil {
			continue
		}
		input := ""
		if probe.ToolInput != nil {
			if cmd, ok := probe.ToolInput["command"].(string); ok {
				input = cmd
			} else if path, ok := probe.ToolInput["file_path"].(string); ok {
				input = path
			} else {
				b, _ := json.Marshal(probe.ToolInput)
				input = string(b)
			}
		}
		out = append(out, ToolCall{Name: probe.ToolName, Input: input})
	}
	return out, rows.Err()
}

func fetchFilesChanged(d *sql.DB, sessionID string) ([]string, error) {
	rows, err := d.Query(`SELECT DISTINCT payload_json FROM events WHERE session_id = ? AND (type = 'file.modified' OR type = 'file.deleted') ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("fetchFilesChanged: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			continue
		}
		var probe struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(p, &probe); err != nil || probe.Path == "" {
			continue
		}
		if !seen[probe.Path] {
			seen[probe.Path] = true
			out = append(out, probe.Path)
		}
	}
	return out, rows.Err()
}

func fetchToolErrors(d *sql.DB, sessionID string) ([]string, error) {
	rows, err := d.Query(`SELECT payload_json FROM events WHERE session_id = ? AND type = 'tool.failed' ORDER BY sequence`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("fetchToolErrors: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			continue
		}
		out = append(out, string(p))
	}
	return out, rows.Err()
}
