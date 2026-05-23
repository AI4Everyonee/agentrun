package redact

import (
	"strings"
	"testing"
)

func TestRegex_RedactsKnownSecrets(t *testing.T) {
	r := NewRegex()

	cases := []struct {
		name      string
		input     string
		wantLabel string  // substring that must appear in output
		mustGoneSubstr string // original secret must NOT appear in output
	}{
		{
			name:           "anthropic key",
			input:          `{"key":"sk-ant-api03-AaBbCcDdEeFfGgHhIiJjKkLlMmNn"}`,
			wantLabel:      "[REDACTED:ANTHROPIC_API_KEY]",
			mustGoneSubstr: "sk-ant-api03-AaBbCcDdEeFfGgHhIiJjKkLlMmNn",
		},
		{
			name:           "openai key with sk-proj prefix",
			input:          `{"key":"sk-proj-abcdefghijklmnopqrstuvwxyzABCDEF"}`,
			wantLabel:      "[REDACTED:OPENAI_API_KEY]",
			mustGoneSubstr: "sk-proj-abcdefghijklmnopqrstuvwxyzABCDEF",
		},
		{
			name:           "openai key plain",
			input:          `auth: sk-abcdefghij1234567890ABCDEFGH`,
			wantLabel:      "[REDACTED:OPENAI_API_KEY]",
			mustGoneSubstr: "sk-abcdefghij1234567890ABCDEFGH",
		},
		{
			name:           "github token classic",
			input:          `token=ghp_AaBbCcDdEeFfGgHhIiJj1234`,
			wantLabel:      "[REDACTED:GITHUB_TOKEN]",
			mustGoneSubstr: "ghp_AaBbCcDdEeFfGgHhIiJj1234",
		},
		{
			name:           "github token fine grained",
			input:          `github_pat_11ABCDEFG0aBcDeFgHiJkLmNoPqRsTuVwXyZ`,
			wantLabel:      "[REDACTED:GITHUB_TOKEN]",
			mustGoneSubstr: "github_pat_11ABCDEFG0aBcDeFgHiJkLmNoPqRsTuVwXyZ",
		},
		{
			name:           "aws access key id",
			input:          `id: AKIAIOSFODNN7EXAMPLE`,
			wantLabel:      "[REDACTED:AWS_ACCESS_KEY_ID]",
			mustGoneSubstr: "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:           "aws secret",
			input:          `aws_secret_access_key = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
			wantLabel:      "[REDACTED:AWS_SECRET_ACCESS_KEY]",
			mustGoneSubstr: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		},
		{
			name:           "slack token",
			input:          `token: xoxb-1234567890-1234567890-AaBbCcDdEeFf`,
			wantLabel:      "[REDACTED:SLACK_TOKEN]",
			mustGoneSubstr: "xoxb-1234567890-1234567890-AaBbCcDdEeFf",
		},
		{
			name:           "jwt",
			input:          `Authorization: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eXoifQ.abc123-_=`,
			wantLabel:      "[REDACTED:JWT]",
			mustGoneSubstr: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eXoifQ.abc123-_",
		},
		{
			name:           "bearer header",
			input:          `Authorization: Bearer abcdef1234567890ABCDEFGHIJ`,
			wantLabel:      "[REDACTED:BEARER]",
			mustGoneSubstr: "abcdef1234567890ABCDEFGHIJ",
		},
		{
			name:           "env-style anthropic",
			input:          `ANTHROPIC_API_KEY=sk-ant-supersecretvalue123456789`,
			wantLabel:      "[REDACTED:",
			mustGoneSubstr: "supersecretvalue123456789",
		},
		{
			name:           "env-style perplexity",
			input:          `PERPLEXITY_API_KEY=pplx-secret1234567890abcdef`,
			wantLabel:      "[REDACTED:ENV_SECRET]",
			mustGoneSubstr: "pplx-secret1234567890abcdef",
		},
		{
			name:           "database url",
			input:          `DATABASE_URL=postgres://user:p4ssw0rd@db.example.com/main`,
			wantLabel:      "[REDACTED:ENV_SECRET]",
			mustGoneSubstr: "postgres://user:p4ssw0rd@db.example.com/main",
		},
		{
			name: "pem private key",
			input: `-----BEGIN RSA PRIVATE KEY-----
MIIEogIBAAKCAQEA1234567890abcdef...stuff...
-----END RSA PRIVATE KEY-----`,
			wantLabel:      "[REDACTED:PRIVATE_KEY]",
			mustGoneSubstr: "MIIEogIBAAKCAQEA",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, ver := r.Redact("test.event", []byte(tc.input))
			if ver != "regex-1" {
				t.Errorf("version = %q, want regex-1", ver)
			}
			outStr := string(out)
			if !strings.Contains(outStr, tc.wantLabel) {
				t.Errorf("output missing %q\ngot: %s", tc.wantLabel, outStr)
			}
			if strings.Contains(outStr, tc.mustGoneSubstr) {
				t.Errorf("output still contains secret %q\ngot: %s", tc.mustGoneSubstr, outStr)
			}
		})
	}
}

func TestRegex_PreservesNonSecretContent(t *testing.T) {
	r := NewRegex()
	cases := []string{
		`{"foo":"bar","baz":42}`,
		`hello world, this is plain text with no secrets`,
		`{"tool_name":"Bash","tool_input":{"command":"echo hello"}}`,
		`session_id: abc-def-123-456`,
		// Short sk- (less than 20 chars after prefix) should NOT match.
		`sku: sk-shortone`,
		// Random base64-ish string that's not a JWT (no two-dot structure) shouldn't match JWT pattern.
		`hash: SGVsbG9Xb3JsZEZvb0Jhcg==`,
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			out, _ := r.Redact("test.event", []byte(in))
			if string(out) != in {
				t.Errorf("non-secret payload was modified\nin:  %s\nout: %s", in, string(out))
			}
		})
	}
}

func TestRegex_PreservesJSONStructure(t *testing.T) {
	r := NewRegex()
	in := `{"key":"sk-ant-secret1234567890abcdef","other":"keep me"}`
	out, _ := r.Redact("test.event", []byte(in))
	s := string(out)
	if !strings.Contains(s, `"other":"keep me"`) {
		t.Errorf("non-secret value lost from JSON: %s", s)
	}
	// Surrounding quote characters should remain.
	if !strings.Contains(s, `"key":"`) {
		t.Errorf("key wrapper quotes were broken: %s", s)
	}
}

func TestNoop_Identity(t *testing.T) {
	in := []byte(`sk-ant-shouldNOTbeRedacted`)
	out, ver := Noop{}.Redact("any", in)
	if string(out) != string(in) {
		t.Errorf("Noop modified payload: %s", out)
	}
	if ver != "noop-1" {
		t.Errorf("Noop version = %q, want noop-1", ver)
	}
}

func TestDefault_ReturnsRegex(t *testing.T) {
	r := Default()
	out, ver := r.Redact("x", []byte("ghp_abcdefghijklmnopqrstuvwxyz12345"))
	if ver != "regex-1" {
		t.Errorf("version = %q, want regex-1", ver)
	}
	if !strings.Contains(string(out), "[REDACTED:GITHUB_TOKEN]") {
		t.Errorf("default redactor didn't redact: %s", out)
	}
}
