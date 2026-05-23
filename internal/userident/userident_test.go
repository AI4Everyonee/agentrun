package userident

import (
	"testing"
)

func TestDetect_AGENTRUN_USER_Wins(t *testing.T) {
	t.Setenv("AGENTRUN_USER", "alice@example.com")
	t.Setenv("USER", "should-be-ignored")
	if got := Detect(); got != "alice@example.com" {
		t.Errorf("Detect() = %q, want alice@example.com", got)
	}
}

func TestDetect_USER_Fallback(t *testing.T) {
	t.Setenv("AGENTRUN_USER", "")
	t.Setenv("USER", "bob")
	t.Setenv("LOGNAME", "")
	if got := Detect(); got != "bob" {
		t.Errorf("Detect() = %q, want bob", got)
	}
}

func TestDetect_LOGNAME_Fallback(t *testing.T) {
	t.Setenv("AGENTRUN_USER", "")
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "carol")
	if got := Detect(); got != "carol" {
		t.Errorf("Detect() = %q, want carol", got)
	}
}

func TestDetect_TrimsWhitespace(t *testing.T) {
	t.Setenv("AGENTRUN_USER", "  dan  ")
	if got := Detect(); got != "dan" {
		t.Errorf("Detect() = %q, want dan (trimmed)", got)
	}
}
