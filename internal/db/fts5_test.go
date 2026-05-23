package db

import (
	"fmt"
	"testing"
	"time"
)

func TestFTS5Search(t *testing.T) {
	d, err := Open(t.TempDir() + "/fts5test.db")
	if err != nil {
		t.Fatal("open:", err)
	}
	defer d.Close()

	sA := SessionRow{ID: "s_filter_A", Agent: "claude", Cwd: "/a", StartedAt: time.Now().UTC(), MetadataJSON: "{}"}
	InsertSession(d, sA)
	InsertSessionSummary(d, "s_filter_A", "completed")
	InsertEventWithAutoSeq(d, "s_filter_A", time.Now().UTC(), "test", "user.prompt", []byte(`{"prompt":"foo is in session A"}`), "")

	sB := SessionRow{ID: "s_filter_B", Agent: "codex", Cwd: "/b", StartedAt: time.Now().UTC(), MetadataJSON: "{}"}
	InsertSession(d, sB)
	InsertSessionSummary(d, "s_filter_B", "completed")
	InsertEventWithAutoSeq(d, "s_filter_B", time.Now().UTC(), "test", "user.prompt", []byte(`{"prompt":"foo is in session B"}`), "")

	hits, err := SearchEvents(d, "foo", "s_filter_A", "", 10)
	if err != nil {
		t.Fatal("SearchEvents:", err)
	}
	fmt.Println("hits:", len(hits))
	for _, h := range hits {
		fmt.Printf("  session=%s type=%s\n", h.SessionID, h.Type)
	}
}
