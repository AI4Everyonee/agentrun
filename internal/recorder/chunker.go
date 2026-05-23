package recorder

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Compile-time check: *Recorder satisfies chunkerEmitter.
var _ chunkerEmitter = (*Recorder)(nil)

const (
	// chunkerByteThreshold is the buffer size at which the chunker flushes immediately.
	chunkerByteThreshold = 4096
	// chunkerTimeWindow is how long after the first byte the chunker waits before flushing
	// if the byte threshold has not been reached. This is a debounce from the FIRST byte,
	// not from the last.
	chunkerTimeWindow = 50 * time.Millisecond
)

// chunkerEmitter is the minimal interface Chunker needs from a Recorder.
// Defined as an interface so chunker tests can use a mock without spinning up a DB.
type chunkerEmitter interface {
	Emit(source, eventType string, payload []byte)
}

// Chunker buffers bytes and emits them as one event per chunk, flushed when either
// (a) the buffer reaches chunkerByteThreshold bytes OR
// (b) chunkerTimeWindow has elapsed since the first byte landed in the buffer.
//
// Use NewChunker for each stream direction (one for terminal.output, one for terminal.stdin).
//
// Payload JSON shape:
//
//	{"bytes_b64":"<base64>","len":<N>}
//
// base64 (not a raw JSON string) is used because PTY bytes routinely include control
// characters (0x00-0x1f, ESC sequences) that require escaping in JSON strings, and
// base64 keeps the JSON valid and round-trippable.
type Chunker struct {
	emitter   chunkerEmitter
	source    string
	eventType string

	mu     sync.Mutex
	buf    []byte
	timer  *time.Timer
	closed bool
}

// NewChunker creates a chunker that emits (source, eventType) events to em.
func NewChunker(em chunkerEmitter, source, eventType string) *Chunker {
	return &Chunker{
		emitter:   em,
		source:    source,
		eventType: eventType,
	}
}

// Write satisfies io.Writer. It copies p into the internal buffer and arranges flushes.
// Never blocks the caller beyond a memcpy + cheap timer management.
func (c *Chunker) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.buf = append(c.buf, p...)

	// If the buffer is at or above the byte threshold, flush in chunkerByteThreshold-sized
	// slices until the remainder is below the threshold.
	for len(c.buf) >= chunkerByteThreshold {
		chunk := c.buf[:chunkerByteThreshold]
		c.emitLocked(chunk)
		c.buf = c.buf[chunkerByteThreshold:]
		// Stop any pending timer since we just flushed.
		if c.timer != nil {
			c.timer.Stop()
			c.timer = nil
		}
	}

	// If there are bytes remaining and no timer is running, start the debounce timer.
	// We intentionally do NOT reset the timer on every Write — the 50ms window starts
	// from the FIRST byte that arrived in this accumulation window (per §2.6).
	if len(c.buf) > 0 && c.timer == nil && !c.closed {
		c.timer = time.AfterFunc(chunkerTimeWindow, c.timerFlush)
	}

	return len(p), nil
}

// Flush emits any pending bytes immediately (under lock).
func (c *Chunker) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.flushLocked()
}

// flushLocked must be called with c.mu held. It emits the current buffer contents
// and stops any pending timer.
func (c *Chunker) flushLocked() {
	if len(c.buf) == 0 {
		return
	}
	c.emitLocked(c.buf)
	c.buf = c.buf[:0]
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

// emitLocked builds the event payload and calls the emitter. Must be called with c.mu held.
func (c *Chunker) emitLocked(data []byte) {
	encoded := base64.StdEncoding.EncodeToString(data)
	payload, err := json.Marshal(struct {
		BytesB64 string `json:"bytes_b64"`
		Len      int    `json:"len"`
	}{
		BytesB64: encoded,
		Len:      len(data),
	})
	if err != nil {
		// json.Marshal of a struct with only string/int fields never errors in practice,
		// but guard defensively.
		fmt.Fprintf(os.Stderr, "agentrun: chunker marshal failed: %v\n", err)
		payload = []byte(fmt.Sprintf(`{"bytes_b64":%q,"len":%d}`, encoded, len(data)))
	}
	c.emitter.Emit(c.source, c.eventType, payload)
}

// timerFlush is called by the AfterFunc timer when the time window expires.
func (c *Chunker) timerFlush() {
	c.Flush()
}

// Close flushes remaining bytes, stops the timer, and marks the chunker closed.
// Further Writes after Close will flush their input as a single chunk and then stop the timer.
func (c *Chunker) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.flushLocked()
	return nil
}
