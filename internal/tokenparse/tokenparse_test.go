package tokenparse

import "testing"

func TestExtract_CodexFormat(t *testing.T) {
	in := StripANSI("session id: abc\ntokens used\n11,728\nDone.")
	got, ok := Extract(in)
	if !ok {
		t.Fatalf("expected match")
	}
	if got.Tokens != 11728 {
		t.Errorf("Tokens = %d, want 11728", got.Tokens)
	}
}

func TestExtract_ClaudeFormat(t *testing.T) {
	in := StripANSI("response complete. tokens: 5,432")
	got, ok := Extract(in)
	if !ok {
		t.Fatalf("expected match")
	}
	if got.Tokens != 5432 {
		t.Errorf("Tokens = %d, want 5432", got.Tokens)
	}
}

func TestExtract_LastMatchWins(t *testing.T) {
	// Multiple "tokens used: N" lines — final value should win (cumulative).
	in := StripANSI("tokens used: 100\nlater\ntokens used: 250")
	got, ok := Extract(in)
	if !ok {
		t.Fatalf("expected match")
	}
	if got.Tokens != 250 {
		t.Errorf("Tokens = %d, want 250 (last wins)", got.Tokens)
	}
}

func TestExtract_NoMatch(t *testing.T) {
	_, ok := Extract("hello world, no tokens info here")
	if ok {
		t.Errorf("expected no match")
	}
}

func TestExtract_Cost(t *testing.T) {
	in := StripANSI("Cost: $0.12 USD")
	got, ok := Extract(in)
	if !ok {
		t.Fatalf("expected match")
	}
	if got.CostCents != 12 {
		t.Errorf("CostCents = %d, want 12", got.CostCents)
	}
}

func TestStripANSI_RemovesEscapes(t *testing.T) {
	in := "\x1b[31mhello\x1b[0m world"
	got := StripANSI(in)
	if got != "hello world" {
		t.Errorf("StripANSI = %q, want %q", got, "hello world")
	}
}

func TestStripANSI_StripsOSC(t *testing.T) {
	// Set window title: ESC ] 0 ; title BEL
	in := "before\x1b]0;my title\x07after"
	got := StripANSI(in)
	if got != "beforeafter" {
		t.Errorf("StripANSI = %q, want %q", got, "beforeafter")
	}
}
