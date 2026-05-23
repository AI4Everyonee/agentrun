package recorder

import (
	"encoding/base64"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// mockEmitter is a thread-safe in-memory emitter for testing.
type mockEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

type emittedEvent struct {
	source    string
	eventType string
	payload   []byte
}

func (m *mockEmitter) Emit(source, eventType string, payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// defensive copy
	p := make([]byte, len(payload))
	copy(p, payload)
	m.events = append(m.events, emittedEvent{source: source, eventType: eventType, payload: p})
}

func (m *mockEmitter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events)
}

func (m *mockEmitter) get(i int) emittedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.events[i]
}

func (m *mockEmitter) all() []emittedEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]emittedEvent, len(m.events))
	copy(out, m.events)
	return out
}

// decodePayload decodes a chunker payload JSON and returns the raw bytes.
func decodePayload(t *testing.T, payload []byte) []byte {
	t.Helper()
	var p struct {
		BytesB64 string `json:"bytes_b64"`
		Len      int    `json:"len"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decodePayload: unmarshal: %v (payload=%q)", err, payload)
	}
	data, err := base64.StdEncoding.DecodeString(p.BytesB64)
	if err != nil {
		t.Fatalf("decodePayload: base64 decode: %v", err)
	}
	if len(data) != p.Len {
		t.Fatalf("decodePayload: len mismatch: got %d, want %d", len(data), p.Len)
	}
	return data
}

// Test 1: Byte threshold flush — writing 5000 bytes triggers one immediate flush of 4096;
// the remaining 904 bytes stay buffered.
func TestChunker_ByteThresholdFlush(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i % 256)
	}

	n, err := c.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != 5000 {
		t.Fatalf("Write returned %d, want 5000", n)
	}

	// Exactly one emit must have happened synchronously (the 4096-byte chunk).
	if got := em.count(); got != 1 {
		t.Fatalf("expected 1 emit after threshold write, got %d", got)
	}
	decoded := decodePayload(t, em.get(0).payload)
	if len(decoded) != chunkerByteThreshold {
		t.Fatalf("first emit len=%d, want %d", len(decoded), chunkerByteThreshold)
	}

	// Wait 10ms — no second emit should happen within this short window.
	time.Sleep(10 * time.Millisecond)
	if got := em.count(); got != 1 {
		t.Fatalf("expected still 1 emit after 10ms, got %d", got)
	}

	c.Close()
}

// Test 2: Multiple-of-threshold — writing 8192 bytes triggers exactly two emits.
func TestChunker_MultipleOfThreshold(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	data := make([]byte, 8192)
	c.Write(data) //nolint:errcheck

	if got := em.count(); got != 2 {
		t.Fatalf("expected 2 emits for 8192 bytes, got %d", got)
	}
	for i := 0; i < 2; i++ {
		decoded := decodePayload(t, em.get(i).payload)
		if len(decoded) != chunkerByteThreshold {
			t.Fatalf("emit[%d] len=%d, want %d", i, len(decoded), chunkerByteThreshold)
		}
	}

	c.Close()
}

// Test 3: Time window flush — 100 bytes written; no emit within 30ms; emit within 70ms.
func TestChunker_TimeWindowFlush(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	data := make([]byte, 100)
	c.Write(data) //nolint:errcheck

	// No emit within 30ms.
	time.Sleep(30 * time.Millisecond)
	if got := em.count(); got != 0 {
		t.Fatalf("expected 0 emits at 30ms, got %d", got)
	}

	// After 70ms total (50ms window has passed), emit should have happened.
	time.Sleep(40 * time.Millisecond) // total ≈ 70ms
	if got := em.count(); got != 1 {
		t.Fatalf("expected 1 emit at ~70ms, got %d", got)
	}
	decoded := decodePayload(t, em.get(0).payload)
	if len(decoded) != 100 {
		t.Fatalf("emit len=%d, want 100", len(decoded))
	}

	c.Close()
}

// Test 4: Close flushes remaining bytes; timer is stopped afterward.
func TestChunker_CloseFlushesRemainder(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	data := make([]byte, 50)
	c.Write(data) //nolint:errcheck

	// No timer-based emit yet.
	if got := em.count(); got != 0 {
		t.Fatalf("expected 0 emits before Close, got %d", got)
	}

	c.Close()

	if got := em.count(); got != 1 {
		t.Fatalf("expected 1 emit after Close, got %d", got)
	}
	decoded := decodePayload(t, em.get(0).payload)
	if len(decoded) != 50 {
		t.Fatalf("emit len=%d, want 50", len(decoded))
	}

	// Wait well past the time window — no second emit.
	time.Sleep(100 * time.Millisecond)
	if got := em.count(); got != 1 {
		t.Fatalf("expected still 1 emit after Close+100ms, got %d", got)
	}
}

// Test 5: Round-trip integrity — bytes containing control characters survive base64 round-trip.
func TestChunker_RoundTripIntegrity(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	input := []byte("hello, world\x00\x01\x02")
	c.Write(input) //nolint:errcheck
	c.Flush()

	if em.count() == 0 {
		t.Fatal("expected at least 1 emit after Flush")
	}

	decoded := decodePayload(t, em.get(0).payload)
	if string(decoded) != string(input) {
		t.Fatalf("round-trip mismatch: got %v, want %v", decoded, input)
	}
}

// Test 6: Concurrent writes — 10 goroutines each writing 1000 bytes; total = 10000 bytes.
func TestChunker_ConcurrentWrites(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	const goroutines = 10
	const bytesEach = 1000

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			data := make([]byte, bytesEach)
			for j := range data {
				data[j] = byte(i)
			}
			c.Write(data) //nolint:errcheck
		}(i)
	}
	wg.Wait()

	c.Close()

	// Sum up all decoded bytes across all emits.
	total := 0
	for _, ev := range em.all() {
		decoded := decodePayload(t, ev.payload)
		total += len(decoded)
	}
	if total != goroutines*bytesEach {
		t.Fatalf("total bytes after concurrent writes = %d, want %d", total, goroutines*bytesEach)
	}
}

// Test 7: Empty Flush does not emit.
func TestChunker_EmptyFlush(t *testing.T) {
	em := &mockEmitter{}
	c := NewChunker(em, "pty", "terminal.output")

	c.Flush()

	if got := em.count(); got != 0 {
		t.Fatalf("Flush on empty chunker emitted %d events, want 0", got)
	}

	c.Close()
}
