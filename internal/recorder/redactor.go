package recorder

// Redactor transforms an event payload before it is persisted.
// Phase 1 ships only NoopRedactor; Phase 2+ may swap in a regex-based implementation.
type Redactor interface {
	Redact(eventType string, payload []byte) (redacted []byte, version string)
}

// NoopRedactor returns its input unchanged.
type NoopRedactor struct{}

// Redact returns payload unchanged with version "noop-1".
func (NoopRedactor) Redact(eventType string, payload []byte) ([]byte, string) {
	return payload, "noop-1"
}
