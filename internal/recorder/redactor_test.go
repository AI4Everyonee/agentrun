package recorder

import (
	"bytes"
	"testing"
)

func TestNoopRedactor(t *testing.T) {
	in := []byte(`{"foo":"bar","secret":"sk-12345"}`)
	out, version := NoopRedactor{}.Redact("any.type", in)
	if !bytes.Equal(in, out) {
		t.Fatalf("NoopRedactor mutated payload: got %q, want %q", out, in)
	}
	if version != "noop-1" {
		t.Fatalf("version = %q, want noop-1", version)
	}
}
