package fswatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mockEmitter captures every Emit call.
type mockEmitter struct {
	mu     sync.Mutex
	events []event
}

type event struct {
	source  string
	evType  string
	payload map[string]any
}

func (m *mockEmitter) Emit(source, evType string, payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p map[string]any
	_ = json.Unmarshal(payload, &p)
	m.events = append(m.events, event{source, evType, p})
}

func (m *mockEmitter) snapshot() []event {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]event, len(m.events))
	copy(out, m.events)
	return out
}

// waitForEvent polls until the predicate matches one of the recorded events
// or timeout elapses.
func waitForEvent(t *testing.T, m *mockEmitter, timeout time.Duration, match func(event) bool) (event, bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, ev := range m.snapshot() {
			if match(ev) {
				return ev, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return event{}, false
}

func TestWatcher_DetectsFileCreate(t *testing.T) {
	root := t.TempDir()
	m := &mockEmitter{}
	w, err := New(root, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	path := filepath.Join(root, "hello.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev, ok := waitForEvent(t, m, 2*time.Second, func(e event) bool {
		return e.evType == "file.modified" && e.payload["path"] == "hello.txt"
	})
	if !ok {
		t.Fatalf("never saw file.modified for hello.txt; events: %+v", m.snapshot())
	}
	if ev.source != "filesystem" {
		t.Errorf("source = %q, want filesystem", ev.source)
	}
	hash, _ := ev.payload["sha256"].(string)
	if hash == "" {
		t.Errorf("expected sha256 in payload, got %+v", ev.payload)
	}
	size, _ := ev.payload["size_bytes"].(float64)
	if int64(size) != 2 {
		t.Errorf("size_bytes = %v, want 2", size)
	}
}

func TestWatcher_IgnoresGitDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}

	m := &mockEmitter{}
	w, err := New(root, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	// Create a file inside .git/.
	if err := os.WriteFile(filepath.Join(root, ".git", "objects", "abc"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// And one outside.
	if err := os.WriteFile(filepath.Join(root, "kept.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, ok := waitForEvent(t, m, 1*time.Second, func(e event) bool {
		return e.evType == "file.modified" && e.payload["path"] == "kept.txt"
	}); !ok {
		t.Fatalf("never saw kept.txt; events: %+v", m.snapshot())
	}

	// Now check that .git events did NOT fire.
	for _, ev := range m.snapshot() {
		if p, _ := ev.payload["path"].(string); p != "" && filepath.HasPrefix(p, ".git") {
			t.Errorf("unexpected event from .git: %+v", ev)
		}
	}
}

func TestWatcher_SkipsBinaryHashing(t *testing.T) {
	root := t.TempDir()
	m := &mockEmitter{}
	w, err := New(root, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	path := filepath.Join(root, "image.png")
	if err := os.WriteFile(path, []byte{0x89, 0x50, 0x4E, 0x47}, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ev, ok := waitForEvent(t, m, 2*time.Second, func(e event) bool {
		return e.evType == "file.modified" && e.payload["path"] == "image.png"
	})
	if !ok {
		t.Fatalf("never saw image.png event; events: %+v", m.snapshot())
	}
	if _, hashPresent := ev.payload["sha256"]; hashPresent {
		t.Errorf("expected no sha256 for binary, got %+v", ev.payload)
	}
}

func TestWatcher_DetectsDelete(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "doomed.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := &mockEmitter{}
	w, err := New(root, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok := waitForEvent(t, m, 2*time.Second, func(e event) bool {
		return e.evType == "file.deleted" && e.payload["path"] == "doomed.txt"
	}); !ok {
		t.Fatalf("never saw file.deleted; events: %+v", m.snapshot())
	}
}

func TestWatcher_IgnoresVimSwapFiles(t *testing.T) {
	root := t.TempDir()
	m := &mockEmitter{}
	w, err := New(root, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(filepath.Join(root, ".main.go.swp"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, ok := waitForEvent(t, m, 2*time.Second, func(e event) bool {
		return e.evType == "file.modified" && e.payload["path"] == "main.go"
	}); !ok {
		t.Fatalf("never saw main.go; events: %+v", m.snapshot())
	}
	for _, ev := range m.snapshot() {
		if p, _ := ev.payload["path"].(string); p == ".main.go.swp" {
			t.Errorf("unexpected event for swap file: %+v", ev)
		}
	}
}

func TestWatcher_CloseIsIdempotent(t *testing.T) {
	root := t.TempDir()
	m := &mockEmitter{}
	w, err := New(root, m)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := w.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close 1: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close 2 (idempotent): %v", err)
	}
}
