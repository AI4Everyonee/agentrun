package summarizer

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSummarize_NoKey_NoOp(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	res, err := Summarize(context.Background(), SessionInput{
		SessionID: "s_x",
		Agent:     "claude",
		Prompts:   []string{"fix the bug"},
	})
	if err != nil {
		t.Fatalf("expected nil error when key absent, got %v", err)
	}
	if res.Summary != "" {
		t.Errorf("expected empty summary when no key, got %q", res.Summary)
	}
}

func TestSummarize_Disabled_NoOp(t *testing.T) {
	t.Setenv("AGENTRUN_SUMMARY_DISABLED", "1")
	t.Setenv("OPENAI_API_KEY", "sk-fake-not-used")
	res, _ := Summarize(context.Background(), SessionInput{Agent: "claude"})
	if res.Summary != "" {
		t.Errorf("expected no summary when disabled, got %q", res.Summary)
	}
}

func TestRenderCorpus_IncludesEverySection(t *testing.T) {
	in := SessionInput{
		Agent:        "claude",
		Cwd:          "/tmp/x",
		RepoRoot:     "/repo/x",
		StartedAt:    time.Now().Add(-5 * time.Minute),
		EndedAt:      time.Now(),
		Prompts:      []string{"refactor auth.go"},
		ToolCalls:    []ToolCall{{Name: "Read", Input: `{"path":"auth.go"}`}, {Name: "Edit", Input: "..."}},
		FilesChanged: []string{"auth.go", "auth_test.go"},
		Errors:       []string{"npm test failed: 3 of 12 specs"},
	}
	out := renderCorpus(in)
	for _, want := range []string{
		"Agent: claude",
		"Repo: /repo/x",
		"User prompts:",
		"refactor auth.go",
		"Tool calls (in order):",
		"Read —",
		"Files changed:",
		"auth_test.go",
		"Errors:",
		"npm test failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("corpus missing %q\n--- corpus ---\n%s", want, out)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("truncate short: %q", got)
	}
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("truncate long: %q", got)
	}
}
