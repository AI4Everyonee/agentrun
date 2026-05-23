package transcript

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AI4Everyonee/agentrun/internal/db"
)

// setupDB creates a temp DB with one session row so Capture's FK constraint is satisfied.
func setupDB(t *testing.T) (*db.SessionRow, string, *os.File) {
	t.Helper()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}

	sess := db.SessionRow{
		ID:           "s_test",
		Agent:        "claude",
		Cwd:          tmp,
		MetadataJSON: "{}",
	}
	if err := db.InsertSession(d, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := db.InsertSessionSummary(d, "s_test", "completed"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}

	// Caller closes; return d as *os.File proxy via Stat. Actually return the
	// values it needs.
	t.Cleanup(func() { d.Close() })
	return &sess, tmp, nil // *os.File slot unused
}

func writeTranscript(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func TestCapture_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "test.db")
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer d.Close()

	sess := db.SessionRow{ID: "s_test", Agent: "claude", Cwd: tmp, MetadataJSON: "{}"}
	if err := db.InsertSession(d, sess); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := db.InsertSessionSummary(d, "s_test", "completed"); err != nil {
		t.Fatalf("InsertSessionSummary: %v", err)
	}

	// JSONL with repeated keys → very compressible.
	original := strings.Repeat(`{"type":"user","content":"hello world"}`+"\n", 1000)
	tPath := writeTranscript(t, tmp, original)

	artDir := filepath.Join(tmp, "artifacts")
	if err := Capture(d, "s_test", tPath, artDir); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Verify the artifact file exists and round-trips.
	dst := filepath.Join(artDir, "s_test", "transcript.jsonl.gz")
	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("open compressed: %v", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != original {
		t.Errorf("content mismatch: got %d bytes, want %d bytes", len(got), len(original))
	}

	// Verify compression actually helped (highly repeated content → tiny output).
	srcSize := int64(len(original))
	dstInfo, _ := os.Stat(dst)
	if dstInfo.Size() >= srcSize/2 {
		t.Errorf("expected significant compression, got %d compressed vs %d raw", dstInfo.Size(), srcSize)
	}

	// Verify artifact row landed.
	arts, err := db.ListArtifacts(d, "s_test")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	found := false
	for _, a := range arts {
		if a.Kind == "transcript" {
			found = true
			if !a.ContentHash.Valid || len(a.ContentHash.String) != 64 {
				t.Errorf("expected SHA-256 hex in ContentHash, got %v", a.ContentHash)
			}
			if !strings.Contains(a.MetadataJSON, "uncompressed_size") {
				t.Errorf("metadata missing uncompressed_size: %s", a.MetadataJSON)
			}
		}
	}
	if !found {
		t.Errorf("transcript artifact row not inserted")
	}
}

func TestCapture_MissingFile_NoOp(t *testing.T) {
	tmp := t.TempDir()
	d, _ := db.Open(filepath.Join(tmp, "test.db"))
	defer d.Close()
	_ = db.InsertSession(d, db.SessionRow{ID: "s_missing", Agent: "claude", Cwd: tmp, MetadataJSON: "{}"})
	_ = db.InsertSessionSummary(d, "s_missing", "completed")

	if err := Capture(d, "s_missing", "/nonexistent/transcript.jsonl", filepath.Join(tmp, "art")); err != nil {
		t.Errorf("expected nil for missing file, got %v", err)
	}
	arts, _ := db.ListArtifacts(d, "s_missing")
	for _, a := range arts {
		if a.Kind == "transcript" {
			t.Errorf("unexpected transcript artifact for missing file")
		}
	}
}

func TestCapture_EmptyPath_NoOp(t *testing.T) {
	tmp := t.TempDir()
	d, _ := db.Open(filepath.Join(tmp, "test.db"))
	defer d.Close()
	if err := Capture(d, "s_x", "", filepath.Join(tmp, "art")); err != nil {
		t.Errorf("expected nil for empty path, got %v", err)
	}
}

func TestCapture_TooLarge_Skipped(t *testing.T) {
	tmp := t.TempDir()
	d, _ := db.Open(filepath.Join(tmp, "test.db"))
	defer d.Close()
	_ = db.InsertSession(d, db.SessionRow{ID: "s_big", Agent: "claude", Cwd: tmp, MetadataJSON: "{}"})
	_ = db.InsertSessionSummary(d, "s_big", "completed")

	// Override cap to 1 MB so we don't actually have to make a 50 MB file.
	t.Setenv("AGENTRUN_MAX_TRANSCRIPT_MB", "1")

	// Write a 2 MB file.
	big := make([]byte, 2*1024*1024)
	for i := range big {
		big[i] = 'a'
	}
	p := filepath.Join(tmp, "big.jsonl")
	if err := os.WriteFile(p, big, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := Capture(d, "s_big", p, filepath.Join(tmp, "art")); err != nil {
		t.Errorf("expected nil for oversized file, got %v", err)
	}
	arts, _ := db.ListArtifacts(d, "s_big")
	for _, a := range arts {
		if a.Kind == "transcript" {
			t.Errorf("unexpected artifact insert for oversized file")
		}
	}
}
