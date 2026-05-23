package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestDB opens a fresh SQLite DB in the test's temp directory.
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// makeSession returns a fully-populated SessionRow for testing.
func makeSession(id string, startedAt time.Time) SessionRow {
	return SessionRow{
		ID:             id,
		Agent:          "claude",
		AgentVersion:   sql.NullString{String: "v1.2.3", Valid: true},
		Model:          sql.NullString{String: "claude-3", Valid: true},
		PermissionMode: sql.NullString{String: "default", Valid: true},
		Cwd:            "/tmp/test",
		RepoRoot:       sql.NullString{String: "/tmp/test", Valid: true},
		Branch:         sql.NullString{String: "main", Valid: true},
		StartCommitSHA: sql.NullString{String: "abcdef01", Valid: true},
		EndCommitSHA:   sql.NullString{String: "deadbeef", Valid: true},
		StartedAt:      startedAt,
		EndedAt:        sql.NullTime{Time: startedAt.Add(time.Hour), Valid: true},
		ExitCode:       sql.NullInt64{Int64: 0, Valid: true},
		TranscriptPath: sql.NullString{String: "/tmp/transcript.json", Valid: true},
		PID:            sql.NullInt64{Int64: 12345, Valid: true},
		MetadataJSON:   "{}",
	}
}

// TestOpenMigrateIdempotency verifies Open succeeds twice on the same path.
func TestOpenMigrateIdempotency(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	defer db1.Close()

	// Second Open on the same file must succeed (Migrate is idempotent via IF NOT EXISTS).
	db2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotency): %v", err)
	}
	defer db2.Close()

	// Migrate on an already-migrated DB must also be a no-op.
	if err := Migrate(db1); err != nil {
		t.Fatalf("second Migrate call: %v", err)
	}
}

// TestInsertGetSessionRoundTrip inserts a session with all fields set and reads it back.
func TestInsertGetSessionRoundTrip(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC().Truncate(time.Nanosecond)
	// Round to microsecond to avoid sub-nanosecond precision loss through RFC3339Nano.
	now = now.Round(time.Microsecond)
	want := makeSession("s_test01", now)

	if err := InsertSession(db, want); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	got, err := GetSession(db, want.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	if got.ID != want.ID {
		t.Errorf("ID: got %q want %q", got.ID, want.ID)
	}
	if got.Agent != want.Agent {
		t.Errorf("Agent: got %q want %q", got.Agent, want.Agent)
	}
	if got.AgentVersion != want.AgentVersion {
		t.Errorf("AgentVersion: got %v want %v", got.AgentVersion, want.AgentVersion)
	}
	if got.Model != want.Model {
		t.Errorf("Model: got %v want %v", got.Model, want.Model)
	}
	if got.PermissionMode != want.PermissionMode {
		t.Errorf("PermissionMode: got %v want %v", got.PermissionMode, want.PermissionMode)
	}
	if got.Cwd != want.Cwd {
		t.Errorf("Cwd: got %q want %q", got.Cwd, want.Cwd)
	}
	if got.RepoRoot != want.RepoRoot {
		t.Errorf("RepoRoot: got %v want %v", got.RepoRoot, want.RepoRoot)
	}
	if got.Branch != want.Branch {
		t.Errorf("Branch: got %v want %v", got.Branch, want.Branch)
	}
	if got.StartCommitSHA != want.StartCommitSHA {
		t.Errorf("StartCommitSHA: got %v want %v", got.StartCommitSHA, want.StartCommitSHA)
	}
	if got.EndCommitSHA != want.EndCommitSHA {
		t.Errorf("EndCommitSHA: got %v want %v", got.EndCommitSHA, want.EndCommitSHA)
	}
	// Time comparison must use .Equal() to be timezone-agnostic.
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt: got %v want %v", got.StartedAt, want.StartedAt)
	}
	if got.EndedAt.Valid != want.EndedAt.Valid {
		t.Errorf("EndedAt.Valid: got %v want %v", got.EndedAt.Valid, want.EndedAt.Valid)
	}
	if got.EndedAt.Valid && !got.EndedAt.Time.Equal(want.EndedAt.Time) {
		t.Errorf("EndedAt.Time: got %v want %v", got.EndedAt.Time, want.EndedAt.Time)
	}
	if got.ExitCode != want.ExitCode {
		t.Errorf("ExitCode: got %v want %v", got.ExitCode, want.ExitCode)
	}
	if got.TranscriptPath != want.TranscriptPath {
		t.Errorf("TranscriptPath: got %v want %v", got.TranscriptPath, want.TranscriptPath)
	}
	if got.PID != want.PID {
		t.Errorf("PID: got %v want %v", got.PID, want.PID)
	}
	if got.MetadataJSON != want.MetadataJSON {
		t.Errorf("MetadataJSON: got %q want %q", got.MetadataJSON, want.MetadataJSON)
	}
}

// TestGetSessionNotFound verifies that GetSession returns sql.ErrNoRows for missing IDs.
func TestGetSessionNotFound(t *testing.T) {
	db := newTestDB(t)

	_, err := GetSession(db, "s_nonexistent")
	if err != sql.ErrNoRows {
		t.Fatalf("GetSession with missing id: got err %v, want sql.ErrNoRows", err)
	}
}

// TestInsertEventOneAndCountByType inserts several events and checks type counts.
func TestInsertEventOneAndCountByType(t *testing.T) {
	db := newTestDB(t)

	// We need a session row to satisfy the FK constraint.
	now := time.Now().UTC().Round(time.Microsecond)
	sess := makeSession("s_evttest", now)
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	events := []EventRow{
		{ID: "evt_1", SessionID: "s_evttest", Sequence: 1, Ts: now, Source: "pty", Type: "terminal.output", PayloadJSON: []byte(`{}`)},
		{ID: "evt_2", SessionID: "s_evttest", Sequence: 2, Ts: now, Source: "pty", Type: "terminal.output", PayloadJSON: []byte(`{}`)},
		{ID: "evt_3", SessionID: "s_evttest", Sequence: 3, Ts: now, Source: "pty", Type: "terminal.output", PayloadJSON: []byte(`{}`)},
		{ID: "evt_4", SessionID: "s_evttest", Sequence: 4, Ts: now, Source: "system", Type: "session.started", PayloadJSON: []byte(`{}`)},
		{ID: "evt_5", SessionID: "s_evttest", Sequence: 5, Ts: now, Source: "system", Type: "session.ended", PayloadJSON: []byte(`{}`)},
	}

	for _, e := range events {
		if err := InsertEventOne(db, e); err != nil {
			t.Fatalf("InsertEventOne(%s): %v", e.ID, err)
		}
	}

	counts, err := CountEventsByType(db, "s_evttest")
	if err != nil {
		t.Fatalf("CountEventsByType: %v", err)
	}

	want := map[string]int{
		"terminal.output": 3,
		"session.started": 1,
		"session.ended":   1,
	}

	for typ, wantCount := range want {
		if counts[typ] != wantCount {
			t.Errorf("type %q: got count %d, want %d", typ, counts[typ], wantCount)
		}
	}
	if len(counts) != len(want) {
		t.Errorf("unexpected extra types in counts: %v", counts)
	}
}

// TestBatchInsertViaInsertEventStmt inserts 100 events in a single transaction and
// verifies the sequences are preserved in order.
func TestBatchInsertViaInsertEventStmt(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)
	sess := makeSession("s_batch", now)
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin tx: %v", err)
	}

	stmt, err := InsertEventStmt(tx)
	if err != nil {
		t.Fatalf("InsertEventStmt: %v", err)
	}

	for i := 1; i <= 100; i++ {
		id := fmt.Sprintf("evt_b%03d", i)
		_, err := stmt.Exec(
			id, "s_batch", int64(i),
			fmtTime(now),
			"pty", "terminal.output",
			[]byte(`{}`),
			nil, // redaction_version
		)
		if err != nil {
			t.Fatalf("stmt.Exec seq=%d: %v", i, err)
		}
	}
	stmt.Close()

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Verify sequences 1..100 in order.
	rows, err := db.Query(`SELECT sequence FROM events WHERE session_id='s_batch' ORDER BY sequence`)
	if err != nil {
		t.Fatalf("query sequences: %v", err)
	}
	defer rows.Close()

	seq := 0
	for rows.Next() {
		seq++
		var got int
		if err := rows.Scan(&got); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		if got != seq {
			t.Errorf("sequence at position %d: got %d, want %d", seq, got, seq)
		}
	}
	if seq != 100 {
		t.Errorf("expected 100 events, got %d", seq)
	}
}

// TestForeignKeyEnforcement verifies that inserting an event with a non-existent
// session_id produces a non-nil error.
func TestForeignKeyEnforcement(t *testing.T) {
	db := newTestDB(t)

	e := EventRow{
		ID:          "evt_fk",
		SessionID:   "s_doesnotexist",
		Sequence:    1,
		Ts:          time.Now().UTC(),
		Source:      "pty",
		Type:        "terminal.output",
		PayloadJSON: []byte(`{}`),
	}

	err := InsertEventOne(db, e)
	if err == nil {
		t.Fatal("expected FK violation error, got nil")
	}
	// The error text varies by driver version but must be non-nil.
	// modernc.org/sqlite reports "FOREIGN KEY constraint failed".
	t.Logf("FK error (expected): %v", err)
}

// TestListSessionsOrdering inserts 3 sessions with distinct start times and verifies
// ListSessions returns them newest-first. Also checks that session_summary.status
// joins correctly.
func TestListSessionsOrdering(t *testing.T) {
	db := newTestDB(t)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ids := []string{"s_old", "s_mid", "s_new"}
	times := []time.Time{
		base,
		base.Add(time.Minute),
		base.Add(2 * time.Minute),
	}

	for i, id := range ids {
		s := makeSession(id, times[i])
		// Clear fields that aren't needed for ordering test.
		s.EndedAt = sql.NullTime{}
		s.ExitCode = sql.NullInt64{}
		s.EndCommitSHA = sql.NullString{}
		if err := InsertSession(db, s); err != nil {
			t.Fatalf("InsertSession %s: %v", id, err)
		}
	}

	// Insert a summary only for the newest session.
	if err := InsertSessionSummary(db, "s_new", "completed"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}

	list, err := ListSessions(db, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	if len(list) != 3 {
		t.Fatalf("ListSessions: got %d rows, want 3", len(list))
	}

	// Must be descending: s_new, s_mid, s_old.
	wantOrder := []string{"s_new", "s_mid", "s_old"}
	for i, r := range list {
		if r.ID != wantOrder[i] {
			t.Errorf("position %d: got %q, want %q", i, r.ID, wantOrder[i])
		}
	}

	// s_new has status "completed".
	if !list[0].Status.Valid || list[0].Status.String != "completed" {
		t.Errorf("s_new status: got %v, want {completed true}", list[0].Status)
	}
	// s_mid and s_old have no summary -> NULL status.
	if list[1].Status.Valid {
		t.Errorf("s_mid status: expected NULL, got %v", list[1].Status)
	}
	if list[2].Status.Valid {
		t.Errorf("s_old status: expected NULL, got %v", list[2].Status)
	}
}

// TestFinalizeSession verifies the two-table update and the COALESCE/NULLIF SHA behavior.
func TestFinalizeSession(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)
	sess := makeSession("s_fin", now)
	sess.EndedAt = sql.NullTime{}
	sess.ExitCode = sql.NullInt64{}
	sess.EndCommitSHA = sql.NullString{}
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := InsertSessionSummary(db, "s_fin", "running"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}

	endedAt := now.Add(time.Hour)
	if err := FinalizeSession(db, "s_fin", "deadbeef", "completed", 0, endedAt); err != nil {
		t.Fatalf("FinalizeSession: %v", err)
	}

	got, err := GetSession(db, "s_fin")
	if err != nil {
		t.Fatalf("GetSession after finalize: %v", err)
	}

	if !got.EndedAt.Valid {
		t.Error("EndedAt should be valid after FinalizeSession")
	}
	if !got.EndedAt.Time.Equal(endedAt) {
		t.Errorf("EndedAt: got %v, want %v", got.EndedAt.Time, endedAt)
	}
	if !got.ExitCode.Valid || got.ExitCode.Int64 != 0 {
		t.Errorf("ExitCode: got %v, want {0 true}", got.ExitCode)
	}
	if !got.EndCommitSHA.Valid || got.EndCommitSHA.String != "deadbeef" {
		t.Errorf("EndCommitSHA: got %v, want {deadbeef true}", got.EndCommitSHA)
	}

	// Call FinalizeSession again with empty SHA — old "deadbeef" must be preserved.
	endedAt2 := endedAt.Add(time.Minute)
	if err := FinalizeSession(db, "s_fin", "", "completed", 0, endedAt2); err != nil {
		t.Fatalf("second FinalizeSession: %v", err)
	}

	got2, err := GetSession(db, "s_fin")
	if err != nil {
		t.Fatalf("GetSession after second finalize: %v", err)
	}
	if !got2.EndCommitSHA.Valid || got2.EndCommitSHA.String != "deadbeef" {
		t.Errorf("EndCommitSHA after empty SHA finalize: got %v, want {deadbeef true}", got2.EndCommitSHA)
	}
}

// TestArtifactInsertUpdateList inserts two artifacts, updates one, and verifies ListArtifacts.
func TestArtifactInsertUpdateList(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)
	sess := makeSession("s_art", now)
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	art1 := ArtifactRow{
		ID:           "art_1",
		SessionID:    "s_art",
		Kind:         "terminal_log",
		Path:         sql.NullString{String: "/tmp/pty.raw", Valid: true},
		MetadataJSON: "{}",
	}
	art2 := ArtifactRow{
		ID:           "art_2",
		SessionID:    "s_art",
		Kind:         "terminal_log",
		Path:         sql.NullString{String: "/tmp/stdin.raw", Valid: true},
		MetadataJSON: "{}",
	}

	if err := InsertArtifact(db, art1); err != nil {
		t.Fatalf("InsertArtifact art1: %v", err)
	}
	if err := InsertArtifact(db, art2); err != nil {
		t.Fatalf("InsertArtifact art2: %v", err)
	}

	// Update art1 with size and hash.
	if err := UpdateArtifactStats(db, "art_1", 123, "abc"); err != nil {
		t.Fatalf("UpdateArtifactStats: %v", err)
	}

	list, err := ListArtifacts(db, "s_art")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListArtifacts: got %d rows, want 2", len(list))
	}

	// Find art1 and art2 by ID.
	byID := make(map[string]ArtifactRow)
	for _, a := range list {
		byID[a.ID] = a
	}

	a1 := byID["art_1"]
	if !a1.SizeBytes.Valid || a1.SizeBytes.Int64 != 123 {
		t.Errorf("art_1 SizeBytes: got %v, want {123 true}", a1.SizeBytes)
	}
	if !a1.ContentHash.Valid || a1.ContentHash.String != "abc" {
		t.Errorf("art_1 ContentHash: got %v, want {abc true}", a1.ContentHash)
	}

	a2 := byID["art_2"]
	if a2.SizeBytes.Valid {
		t.Errorf("art_2 SizeBytes: expected NULL, got %v", a2.SizeBytes)
	}
	if a2.ContentHash.Valid {
		t.Errorf("art_2 ContentHash: expected NULL, got %v", a2.ContentHash)
	}
}

// ---------------------------------------------------------------------------
// Phase 2 tests — InsertEventWithAutoSeq, IncrementSummaryCounter,
// UpdateSessionModel, UpdateSessionTranscriptPath, OpenReadWrite
// ---------------------------------------------------------------------------

// newTestDBWithSession opens a fresh DB and inserts one session + session_summary row.
// Returns the db and the session id. Fails the test on any error.
func newTestDBWithSession(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db := newTestDB(t)
	now := time.Now().UTC().Round(time.Microsecond)
	sid := "s_p2test"
	sess := makeSession(sid, now)
	// Clear transcript_path so we can test UpdateSessionTranscriptPath separately.
	sess.TranscriptPath = sql.NullString{}
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("newTestDBWithSession: InsertSession: %v", err)
	}
	if err := InsertSessionSummary(db, sid, "running"); err != nil {
		t.Fatalf("newTestDBWithSession: InsertSessionSummary: %v", err)
	}
	return db, sid
}

// TestInsertEventWithAutoSeq_AssignsConsecutiveSeq inserts 5 events via
// InsertEventWithAutoSeq and verifies the sequences are 1,2,3,4,5.
func TestInsertEventWithAutoSeq_AssignsConsecutiveSeq(t *testing.T) {
	db, sid := newTestDBWithSession(t)

	now := time.Now().UTC()
	var ids []string
	for i := 0; i < 5; i++ {
		evtID, err := InsertEventWithAutoSeq(db, sid, now, "hook", "tool.pre_use", []byte(`{}`), "noop-1")
		if err != nil {
			t.Fatalf("InsertEventWithAutoSeq i=%d: %v", i, err)
		}
		if evtID == "" {
			t.Fatalf("InsertEventWithAutoSeq i=%d: returned empty event ID", i)
		}
		ids = append(ids, evtID)
	}

	// Each returned ID must be unique.
	seen := make(map[string]bool)
	for _, id := range ids {
		if seen[id] {
			t.Errorf("duplicate event ID returned: %q", id)
		}
		seen[id] = true
	}

	// Sequences must be 1,2,3,4,5 in order.
	rows, err := db.Query(`SELECT sequence FROM events WHERE session_id=? ORDER BY sequence`, sid)
	if err != nil {
		t.Fatalf("query sequences: %v", err)
	}
	defer rows.Close()

	wantSeq := int64(1)
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		if seq != wantSeq {
			t.Errorf("sequence: got %d, want %d", seq, wantSeq)
		}
		wantSeq++
	}
	if wantSeq != 6 {
		t.Errorf("expected 5 events, counted %d", wantSeq-1)
	}
}

// TestInsertEventWithAutoSeq_ConcurrentNoCollisions spawns 50 goroutines, each
// inserting 4 events (200 total) against the same session_id. Verifies no UNIQUE
// constraint violations leaked to callers, COUNT(*)==200, and all sequences are
// distinct.
//
// Note: MAX(sequence) >= 200 but gaps are allowed under contention — a retry
// re-reads MAX and may skip a slot. The UNIQUE constraint guarantees no
// duplicates; it does not guarantee a dense sequence.
//
// Each goroutine opens its own *sql.DB via OpenReadWrite (SetMaxOpenConns=1)
// to mirror the production model where each hook invocation is a separate OS
// process with its own connection. This ensures the busy_timeout + retry
// envelope gets exercised rather than goroutines stacking up inside the same
// connection pool.
func TestInsertEventWithAutoSeq_ConcurrentNoCollisions(t *testing.T) {
	// First, create and migrate the DB using db.Open.
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.db")
	seedDB, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	now := time.Now().UTC().Round(time.Microsecond)
	sid := "s_concurrent"
	sess := makeSession(sid, now)
	sess.TranscriptPath = sql.NullString{}
	if err := InsertSession(seedDB, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := InsertSessionSummary(seedDB, sid, "running"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}
	seedDB.Close()

	const goroutines = 50
	const eventsPerGoroutine = 4
	const total = goroutines * eventsPerGoroutine

	errs := make(chan error, total)
	var wg sync.WaitGroup
	nowEvt := time.Now().UTC()

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			// Each goroutine opens its own connection, mirroring a separate
			// hook subprocess. SetMaxOpenConns(1) ensures serialization at
			// the SQLite level and lets busy_timeout do its job.
			d, err := OpenReadWrite(path)
			if err != nil {
				errs <- fmt.Errorf("OpenReadWrite: %w", err)
				return
			}
			defer d.Close()
			for i := 0; i < eventsPerGoroutine; i++ {
				_, err := InsertEventWithAutoSeq(d, sid, nowEvt, "hook", "tool.pre_use", []byte(`{}`), "noop-1")
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("goroutine insert error: %v", err)
	}

	// Reopen for reads.
	readDB, err := OpenReadWrite(path)
	if err != nil {
		t.Fatalf("OpenReadWrite for read: %v", err)
	}
	defer readDB.Close()

	// Count must equal total.
	var count int
	if err := readDB.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id=?`, sid).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != total {
		t.Errorf("event count: got %d, want %d", count, total)
	}

	// MAX(sequence) must be >= total.
	var maxSeq int64
	if err := readDB.QueryRow(`SELECT MAX(sequence) FROM events WHERE session_id=?`, sid).Scan(&maxSeq); err != nil {
		t.Fatalf("max seq query: %v", err)
	}
	if maxSeq < int64(total) {
		t.Errorf("MAX(sequence)=%d, want >= %d", maxSeq, total)
	}

	// No two rows may share the same sequence (UNIQUE constraint preserved).
	rows, err := readDB.Query(`SELECT sequence FROM events WHERE session_id=? ORDER BY sequence`, sid)
	if err != nil {
		t.Fatalf("sequence scan query: %v", err)
	}
	defer rows.Close()

	var prev int64 = -1
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan sequence: %v", err)
		}
		if seq == prev {
			t.Errorf("duplicate sequence %d found", seq)
		}
		prev = seq
	}
}

// TestIncrementSummaryCounter_AllowlistRejectsBadInput verifies that columns
// not in the allowlist return a non-nil error mentioning "allowlist", while an
// allowed column succeeds and the DB value is incremented.
func TestIncrementSummaryCounter_AllowlistRejectsBadInput(t *testing.T) {
	db, sid := newTestDBWithSession(t)

	// Bad column must be rejected.
	err := IncrementSummaryCounter(db, sid, "exit_code")
	if err == nil {
		t.Fatal("expected error for disallowed counter, got nil")
	}
	if !errContains(err, "allowlist") {
		t.Errorf("error %q should mention 'allowlist'", err.Error())
	}

	// Good column must succeed.
	if err := IncrementSummaryCounter(db, sid, "tool_calls"); err != nil {
		t.Fatalf("IncrementSummaryCounter(tool_calls): %v", err)
	}

	// Verify tool_calls == 1.
	var tc int
	if err := db.QueryRow(`SELECT tool_calls FROM session_summary WHERE session_id=?`, sid).Scan(&tc); err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	if tc != 1 {
		t.Errorf("tool_calls: got %d, want 1", tc)
	}
}

// TestIncrementSummaryCounter_AtomicUnderConcurrency runs 100 goroutines each
// calling IncrementSummaryCounter for tool_calls. The final value must be 100.
func TestIncrementSummaryCounter_AtomicUnderConcurrency(t *testing.T) {
	db, sid := newTestDBWithSession(t)

	const goroutines = 100
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			if err := IncrementSummaryCounter(db, sid, "tool_calls"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("goroutine increment error: %v", err)
	}

	var tc int
	if err := db.QueryRow(`SELECT tool_calls FROM session_summary WHERE session_id=?`, sid).Scan(&tc); err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	if tc != goroutines {
		t.Errorf("tool_calls: got %d, want %d", tc, goroutines)
	}
}

// TestUpdateSessionModel_RoundTrip verifies UpdateSessionModel sets the model
// column, and that an empty-string call is a no-op.
func TestUpdateSessionModel_RoundTrip(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)
	sid := "s_modeltest"
	sess := makeSession(sid, now)
	sess.Model = sql.NullString{} // start with empty model
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Set the model.
	if err := UpdateSessionModel(db, sid, "claude-sonnet-4-7"); err != nil {
		t.Fatalf("UpdateSessionModel: %v", err)
	}

	got, err := GetSession(db, sid)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !got.Model.Valid || got.Model.String != "claude-sonnet-4-7" {
		t.Errorf("Model: got %v, want {claude-sonnet-4-7 true}", got.Model)
	}

	// Empty-string call must be a no-op (returns nil, does not overwrite).
	if err := UpdateSessionModel(db, sid, ""); err != nil {
		t.Fatalf("UpdateSessionModel empty: %v", err)
	}

	got2, err := GetSession(db, sid)
	if err != nil {
		t.Fatalf("GetSession after empty call: %v", err)
	}
	if !got2.Model.Valid || got2.Model.String != "claude-sonnet-4-7" {
		t.Errorf("Model after empty call: got %v, want {claude-sonnet-4-7 true}", got2.Model)
	}
}

// TestUpdateSessionTranscriptPath_NullGuard verifies the first writer wins
// (WHERE transcript_path IS NULL guard) and empty path is a no-op.
func TestUpdateSessionTranscriptPath_NullGuard(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)
	sid := "s_tptest"
	sess := makeSession(sid, now)
	sess.TranscriptPath = sql.NullString{} // start NULL
	if err := InsertSession(db, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// First write — should succeed.
	if err := UpdateSessionTranscriptPath(db, sid, "/tmp/a.jsonl"); err != nil {
		t.Fatalf("UpdateSessionTranscriptPath first: %v", err)
	}

	got, err := GetSession(db, sid)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !got.TranscriptPath.Valid || got.TranscriptPath.String != "/tmp/a.jsonl" {
		t.Errorf("TranscriptPath: got %v, want {/tmp/a.jsonl true}", got.TranscriptPath)
	}

	// Second write with a different path — returns nil but WHERE IS NULL prevents overwrite.
	if err := UpdateSessionTranscriptPath(db, sid, "/tmp/b.jsonl"); err != nil {
		t.Fatalf("UpdateSessionTranscriptPath second: %v", err)
	}

	got2, err := GetSession(db, sid)
	if err != nil {
		t.Fatalf("GetSession after second write: %v", err)
	}
	if !got2.TranscriptPath.Valid || got2.TranscriptPath.String != "/tmp/a.jsonl" {
		t.Errorf("TranscriptPath after second write: got %v, want {/tmp/a.jsonl true} (first wins)", got2.TranscriptPath)
	}

	// Empty-string call must be a no-op (returns nil).
	if err := UpdateSessionTranscriptPath(db, sid, ""); err != nil {
		t.Fatalf("UpdateSessionTranscriptPath empty: %v", err)
	}
}

// TestOpenReadWrite_Basic verifies that OpenReadWrite can read and write an
// existing, migrated database.
func TestOpenReadWrite_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rw_test.db")

	// Create and migrate via db.Open.
	db1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	now := time.Now().UTC().Round(time.Microsecond)
	sid := "s_rwtest"
	sess := makeSession(sid, now)
	sess.TranscriptPath = sql.NullString{}
	if err := InsertSession(db1, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := InsertSessionSummary(db1, sid, "running"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}
	db1.Close()

	// Reopen without migration.
	db2, err := OpenReadWrite(path)
	if err != nil {
		t.Fatalf("OpenReadWrite: %v", err)
	}
	defer db2.Close()

	// Reading must work.
	gotSess, err := GetSession(db2, sid)
	if err != nil {
		t.Fatalf("GetSession via OpenReadWrite: %v", err)
	}
	if gotSess.ID != sid {
		t.Errorf("session ID: got %q, want %q", gotSess.ID, sid)
	}

	// Writing must work.
	if err := IncrementSummaryCounter(db2, sid, "tool_calls"); err != nil {
		t.Fatalf("IncrementSummaryCounter via OpenReadWrite: %v", err)
	}

	var tc int
	if err := db2.QueryRow(`SELECT tool_calls FROM session_summary WHERE session_id=?`, sid).Scan(&tc); err != nil {
		t.Fatalf("query tool_calls: %v", err)
	}
	if tc != 1 {
		t.Errorf("tool_calls: got %d, want 1", tc)
	}
}

// ---------------------------------------------------------------------------
// Phase 6 tests — FetchSessionEvents, FinalizeIdleSessions
// ---------------------------------------------------------------------------

// TestFetchSessionEvents_Order inserts 5 events with out-of-order sequences and
// verifies FetchSessionEvents returns them sorted by sequence ascending.
func TestFetchSessionEvents_Order(t *testing.T) {
	d := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)
	sess := makeSession("s_fetchorder", now)
	if err := InsertSession(d, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// Insert events with deliberately shuffled sequences.
	seqsToInsert := []int64{5, 1, 3, 2, 4}
	for _, seq := range seqsToInsert {
		e := EventRow{
			ID:          fmt.Sprintf("evt_ord%d", seq),
			SessionID:   "s_fetchorder",
			Sequence:    seq,
			Ts:          now,
			Source:      "hook",
			Type:        "tool.pre_use",
			PayloadJSON: []byte(`{}`),
		}
		if err := InsertEventOne(d, e); err != nil {
			t.Fatalf("InsertEventOne seq=%d: %v", seq, err)
		}
	}

	events, err := FetchSessionEvents(d, "s_fetchorder")
	if err != nil {
		t.Fatalf("FetchSessionEvents: %v", err)
	}

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}

	// Must be in ascending sequence order: 1, 2, 3, 4, 5.
	for i, e := range events {
		wantSeq := int64(i + 1)
		if e.Sequence != wantSeq {
			t.Errorf("events[%d].Sequence = %d, want %d", i, e.Sequence, wantSeq)
		}
	}
}

// TestFinalizeIdleSessions_OnlyAffectsOldSessions sets up two running sessions,
// one with a recent event and one with an old event, sweeps with olderThan=10m,
// and asserts only the old one is finalized.
func TestFinalizeIdleSessions_OnlyAffectsOldSessions(t *testing.T) {
	d := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)

	// Session A: recent event (1 minute ago) — should NOT be finalized.
	sessA := makeSession("s_recent", now)
	sessA.EndedAt = sql.NullTime{}
	sessA.ExitCode = sql.NullInt64{}
	sessA.EndCommitSHA = sql.NullString{}
	if err := InsertSession(d, sessA); err != nil {
		t.Fatalf("InsertSession sessA: %v", err)
	}
	if err := InsertSessionSummary(d, "s_recent", "running"); err != nil {
		t.Fatalf("InsertSessionSummary sessA: %v", err)
	}
	recentEvt := EventRow{
		ID:          "evt_recent",
		SessionID:   "s_recent",
		Sequence:    1,
		Ts:          now.Add(-1 * time.Minute),
		Source:      "hook",
		Type:        "tool.pre_use",
		PayloadJSON: []byte(`{}`),
	}
	if err := InsertEventOne(d, recentEvt); err != nil {
		t.Fatalf("InsertEventOne recent: %v", err)
	}

	// Session B: old event (60 minutes ago) — should be finalized.
	sessB := makeSession("s_old_idle", now)
	sessB.EndedAt = sql.NullTime{}
	sessB.ExitCode = sql.NullInt64{}
	sessB.EndCommitSHA = sql.NullString{}
	if err := InsertSession(d, sessB); err != nil {
		t.Fatalf("InsertSession sessB: %v", err)
	}
	if err := InsertSessionSummary(d, "s_old_idle", "running"); err != nil {
		t.Fatalf("InsertSessionSummary sessB: %v", err)
	}
	oldEvt := EventRow{
		ID:          "evt_old",
		SessionID:   "s_old_idle",
		Sequence:    1,
		Ts:          now.Add(-60 * time.Minute),
		Source:      "hook",
		Type:        "tool.pre_use",
		PayloadJSON: []byte(`{}`),
	}
	if err := InsertEventOne(d, oldEvt); err != nil {
		t.Fatalf("InsertEventOne old: %v", err)
	}

	count, err := FinalizeIdleSessions(d, 10*time.Minute)
	if err != nil {
		t.Fatalf("FinalizeIdleSessions: %v", err)
	}

	if count != 1 {
		t.Errorf("FinalizeIdleSessions: got count=%d, want 1", count)
	}

	// s_recent must still be running.
	var statusRecent string
	if err := d.QueryRow(`SELECT status FROM session_summary WHERE session_id='s_recent'`).Scan(&statusRecent); err != nil {
		t.Fatalf("query s_recent status: %v", err)
	}
	if statusRecent != "running" {
		t.Errorf("s_recent status: got %q, want 'running'", statusRecent)
	}

	// s_old_idle must now be completed.
	var statusOld string
	if err := d.QueryRow(`SELECT status FROM session_summary WHERE session_id='s_old_idle'`).Scan(&statusOld); err != nil {
		t.Fatalf("query s_old_idle status: %v", err)
	}
	if statusOld != "completed" {
		t.Errorf("s_old_idle status: got %q, want 'completed'", statusOld)
	}
}

// TestFinalizeIdleSessions_NoChangeWhenAllRecent verifies that when all events
// are recent, the sweep returns count==0 and no statuses are changed.
func TestFinalizeIdleSessions_NoChangeWhenAllRecent(t *testing.T) {
	d := newTestDB(t)

	now := time.Now().UTC().Round(time.Microsecond)

	sess := makeSession("s_fresh", now)
	sess.EndedAt = sql.NullTime{}
	sess.ExitCode = sql.NullInt64{}
	sess.EndCommitSHA = sql.NullString{}
	if err := InsertSession(d, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := InsertSessionSummary(d, "s_fresh", "running"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}
	freshEvt := EventRow{
		ID:          "evt_fresh",
		SessionID:   "s_fresh",
		Sequence:    1,
		Ts:          now.Add(-1 * time.Minute),
		Source:      "hook",
		Type:        "tool.pre_use",
		PayloadJSON: []byte(`{}`),
	}
	if err := InsertEventOne(d, freshEvt); err != nil {
		t.Fatalf("InsertEventOne fresh: %v", err)
	}

	count, err := FinalizeIdleSessions(d, 30*time.Minute)
	if err != nil {
		t.Fatalf("FinalizeIdleSessions: %v", err)
	}

	if count != 0 {
		t.Errorf("FinalizeIdleSessions: got count=%d, want 0", count)
	}

	var status string
	if err := d.QueryRow(`SELECT status FROM session_summary WHERE session_id='s_fresh'`).Scan(&status); err != nil {
		t.Fatalf("query s_fresh status: %v", err)
	}
	if status != "running" {
		t.Errorf("s_fresh status: got %q, want 'running'", status)
	}
}

// TestOpenReadWrite_MissingFile verifies that OpenReadWrite on a missing file
// "fails loudly" — at minimum any subsequent query returns a non-nil error.
// (modernc.org/sqlite may not error on Open itself for missing files — it's
// lazy about file creation — so we verify the query fails.)
func TestOpenReadWrite_MissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.db")

	// Open may or may not fail (SQLite is lazy).
	db, err := OpenReadWrite(path)
	if err != nil {
		// If Open itself fails, the test passes.
		t.Logf("OpenReadWrite returned error (acceptable): %v", err)
		return
	}
	defer db.Close()

	// Any query on an unmigrated/missing-schema DB must fail.
	_, err = GetSession(db, "nonexistent")
	if err == nil {
		t.Fatal("expected error from GetSession on missing-schema DB, got nil")
	}
	t.Logf("GetSession on missing-schema DB returned (expected) error: %v", err)
}
