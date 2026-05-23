// Package redact provides best-effort secret redaction for event payloads
// recorded by agentrun. It operates on raw bytes and preserves JSON structure
// by substituting matched substrings with a typed placeholder.
//
// Phase 7 ships a Regex redactor covering the highest-signal patterns:
// Anthropic / OpenAI / GitHub / AWS keys, JWTs, common .env-style assignments,
// and bearer tokens. The implementation is conservative — false positives are
// preferred over false negatives where the choice is forced, but expensive
// patterns (e.g. ASN.1 PEM detection) are deferred until measured to matter.
//
// Limitations (documented, not bugs):
//   - Base64-encoded PTY payloads are NOT decoded for redaction. Terminal
//     events ride a different write path and are still subject to the
//     artifact-on-disk fallback. If a secret echoes to the TUI the bytes are
//     captured verbatim. Use Claude's own permission system to prevent that.
//   - We do not parse JSON. Substitution is byte-level on the raw payload.
//     This keeps the hot path cheap and tolerant of malformed payloads.
package redact

import (
	"regexp"
)

// Redactor scrubs secrets from a payload. Implementations return the redacted
// bytes and a stable version string written to events.redaction_version so
// downstream consumers know which ruleset was applied.
type Redactor interface {
	Redact(eventType string, payload []byte) (redacted []byte, version string)
}

// Noop returns the payload unchanged. Useful for tests.
type Noop struct{}

// Redact satisfies Redactor with no behavior. Version: "noop-1".
func (Noop) Redact(eventType string, payload []byte) ([]byte, string) {
	return payload, "noop-1"
}

// Default returns the production redactor — the Regex implementation with the
// current shipping ruleset.
func Default() Redactor {
	return defaultRegex
}

// pattern bundles a compiled regex with the placeholder label it emits.
type pattern struct {
	re    *regexp.Regexp
	label string
}

// Regex applies a fixed set of patterns. Patterns are compiled once at package
// init; Redact only runs ReplaceAll per call. Zero allocations beyond the
// substring slice copies done by ReplaceAllFunc.
type Regex struct {
	patterns []pattern
	version  string
}

// NewRegex returns the default Regex redactor.
func NewRegex() *Regex {
	return defaultRegex
}

var defaultRegex = &Regex{
	version: "regex-1",
	patterns: []pattern{
		// Anthropic API keys (sk-ant-...).
		{regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`), "ANTHROPIC_API_KEY"},
		// OpenAI keys (sk-..., sk-proj-...).
		{regexp.MustCompile(`sk-proj-[A-Za-z0-9_\-]{20,}`), "OPENAI_API_KEY"},
		{regexp.MustCompile(`\bsk-[A-Za-z0-9]{20,}\b`), "OPENAI_API_KEY"},
		// GitHub tokens (classic + fine-grained).
		{regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`), "GITHUB_TOKEN"},
		{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`), "GITHUB_TOKEN"},
		// AWS access key IDs + secret access keys.
		{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AWS_ACCESS_KEY_ID"},
		{regexp.MustCompile(`(?i)aws_secret_access_key\s*[:=]\s*['"]?[A-Za-z0-9/+=]{40}['"]?`), "AWS_SECRET_ACCESS_KEY"},
		// Slack tokens.
		{regexp.MustCompile(`xox[abprsu]-[A-Za-z0-9\-]{10,}`), "SLACK_TOKEN"},
		// JWTs (header.payload.signature).
		{regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`), "JWT"},
		// Bearer tokens in Authorization headers.
		{regexp.MustCompile(`(?i)(authorization\s*[:=]\s*['"]?bearer\s+)([A-Za-z0-9_\-\.]{16,})`), "BEARER"},
		// Common .env-style assignments. Catches both quoted and unquoted values.
		{regexp.MustCompile(`(?i)((?:^|[\s,])(?:ANTHROPIC_API_KEY|OPENAI_API_KEY|HUGGINGFACE_TOKEN|HF_TOKEN|GROQ_API_KEY|GEMINI_API_KEY|GOOGLE_API_KEY|MISTRAL_API_KEY|COHERE_API_KEY|REPLICATE_API_TOKEN|PERPLEXITY_API_KEY|API_KEY|API_TOKEN|SECRET_KEY|PRIVATE_KEY|DATABASE_URL|DB_PASSWORD|PASSWORD|REDIS_URL|MONGODB_URI)\s*[:=]\s*['"]?)([^\s'"]{8,})(['"]?)`), "ENV_SECRET"},
		// PEM private key blocks.
		{regexp.MustCompile(`-----BEGIN [A-Z ]+PRIVATE KEY-----[\s\S]+?-----END [A-Z ]+PRIVATE KEY-----`), "PRIVATE_KEY"},
	},
}

// Redact runs every compiled pattern against payload, replacing each match
// with "[REDACTED:<label>]". For patterns that capture surrounding context
// (the .env-style assignment and Authorization-bearer pattern), we preserve
// the prefix/suffix groups so the surrounding structure stays intact.
func (r *Regex) Redact(eventType string, payload []byte) ([]byte, string) {
	out := payload
	for _, p := range r.patterns {
		out = p.re.ReplaceAllFunc(out, func(match []byte) []byte {
			// Patterns that include capture groups for surrounding context:
			// rebuild from submatches so we keep prefix + replace value.
			if p.re.NumSubexp() >= 2 {
				groups := p.re.FindSubmatchIndex(match)
				if len(groups) >= 6 {
					prefix := match[groups[2]:groups[3]]
					// Last group (if present) is the closing quote; preserve it.
					suffix := []byte{}
					if len(groups) >= 8 && groups[6] >= 0 {
						suffix = match[groups[6]:groups[7]]
					}
					replacement := append(prefix, []byte("[REDACTED:"+p.label+"]")...)
					return append(replacement, suffix...)
				}
			}
			return []byte("[REDACTED:" + p.label + "]")
		})
	}
	return out, r.version
}
