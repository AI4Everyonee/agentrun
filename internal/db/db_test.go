package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
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
