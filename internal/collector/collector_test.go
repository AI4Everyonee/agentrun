package collector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/collector"
	"github.com/AI4Everyonee/agentrun/internal/db"
)

// tempDB creates a fresh SQLite database in a temp dir and returns its path.
func tempDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open temp db: %v", err)
	}
	d.Close()
	return dbPath
}

// startServer creates and starts a collector.Server on a temp socket with a temp DB.
// Returns the server and the paths so tests can send events and inspect the DB.
func startServer(t *testing.T) (srv *collector.Server, socketPath, dbPath string) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "test.sock")
	dbPath = tempDB(t)

	var err error
	srv, err = collector.New(socketPath, dbPath)
	if err != nil {
		t.Fatalf("collector.New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("srv.Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return srv, socketPath, dbPath
}

// countEvents returns the number of events for a session in the DB.
func countEvents(t *testing.T, dbPath, sessionID string) int {
	t.Helper()
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	var n int
	row := d.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = ?`, sessionID)
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// ensureSessionExists inserts a minimal session + summary row so event FKs work.
func ensureSessionExists(t *testing.T, dbPath, sessionID string) {
	t.Helper()
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	sess := db.SessionRow{
		ID:           sessionID,
		Agent:        "claude",
		Cwd:          "/tmp",
		StartedAt:    time.Now().UTC(),
		MetadataJSON: `{}`,
	}
	if err := db.InsertSession(d, sess); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	if err := db.InsertSessionSummary(d, sessionID, "running"); err != nil {
		t.Fatalf("insert session summary: %v", err)
	}
}

// TestCollector_RoundTrip verifies that a single event sent via TrySend
// lands in the database.
func TestCollector_RoundTrip(t *testing.T) {
	_, socketPath, dbPath := startServer(t)

	const sessionID = "s_test_roundtrip"
	ensureSessionExists(t, dbPath, sessionID)

	payload := json.RawMessage(`{"session_id":"roundtrip-abc"}`)
	req := collector.Request{
		Agent:     "claude",
		Event:     "SessionStart",
		SessionID: sessionID,
		Payload:   payload,
		Native:    false,
	}

	if err := collector.TrySend(socketPath, req); err != nil {
		t.Fatalf("TrySend: %v", err)
	}

	// Give the server a moment to complete the insert.
	time.Sleep(50 * time.Millisecond)

	n := countEvents(t, dbPath, sessionID)
	if n == 0 {
		t.Fatal("expected at least one event in db after round-trip, got 0")
	}
}

// TestCollector_FallbackOnNoSocket verifies that TrySend returns an error quickly
// when no socket exists.
func TestCollector_FallbackOnNoSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "nonexistent.sock")

	start := time.Now()
	err := collector.TrySend(socketPath, collector.Request{
		Agent:     "claude",
		Event:     "PreToolUse",
		SessionID: "s_test_fallback",
		Payload:   json.RawMessage(`{}`),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from TrySend to non-existent socket, got nil")
	}

	const maxWait = 100 * time.Millisecond
	if elapsed > maxWait {
		t.Fatalf("TrySend to missing socket took %v, expected < %v", elapsed, maxWait)
	}
}

// TestCollector_ShutdownDrains starts a server, fires 10 events concurrently,
// shuts down, then verifies all 10 rows are in the DB.
func TestCollector_ShutdownDrains(t *testing.T) {
	srv, socketPath, dbPath := startServer(t)

	const (
		numEvents = 10
		sessionID = "s_test_drain"
	)
	ensureSessionExists(t, dbPath, sessionID)

	var wg sync.WaitGroup
	errs := make([]error, numEvents)
	for i := 0; i < numEvents; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := collector.Request{
				Agent:     "claude",
				Event:     "PreToolUse",
				SessionID: sessionID,
				Payload:   json.RawMessage(fmt.Sprintf(`{"i":%d}`, idx)),
				Native:    false,
			}
			errs[idx] = collector.TrySend(socketPath, req)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("TrySend[%d]: %v", i, err)
		}
	}

	// Shutdown and wait for all in-flight handlers.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Verify all events landed.
	n := countEvents(t, dbPath, sessionID)
	if n != numEvents {
		t.Fatalf("expected %d events in db after drain, got %d", numEvents, n)
	}
}

// ensureDir is a helper for tests that need a directory.
func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
