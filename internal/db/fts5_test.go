package db

import (
	"fmt"
	"testing"
)

func TestFTS5Search(t *testing.T) {
	d, err := Open(t.TempDir() + "/fts5test.db")
	if err != nil {
		t.Fatal("open:", err)
	}
	defer d.Close()

	d.Exec(`INSERT INTO sessions (id, agent, cwd, started_at, metadata_json) VALUES ('s_test1', 'claude', '/tmp', '2026-01-01T00:00:00Z', '{}')`)
	d.Exec(`INSERT INTO session_summary (session_id, status) VALUES ('s_test1', 'completed')`)

	d.Exec(`INSERT INTO events (id, session_id, sequence, ts, source, type, payload_json) VALUES ('evt1', 's_test1', 1, '2026-01-01T00:00:00Z', 'test', 'user.prompt', '{"prompt":"rate limit hit here"}')`)

	// The FTS5 content table reads 'payload' column from events — but our column is 'payload_json'.
	// So snippet() will fail. Let's try without the content lookup.
	// Option A: explicit content table lookup with correct column name mapping
	// The FTS5 schema uses content='events' and payload -> stored as FTS column
	// When FTS5 uses content tables, snippet() reads T.payload from events table.
	// Since our events table has payload_json not payload, snippet() fails.

	// Try: can we use snippet on a non-content FTS table? No, content_rowid means content table.

	// Try: use fts5 with no content table and store payload directly
	// But that's a schema change...

	// Try using highlight/snippet with a query that doesn't require content read-back
	// This won't work either.

	// Check what happens with a simple FTS query (no snippet)
	rows, err := d.Query(`
		SELECT e.id, e.session_id, s.agent, e.type, e.ts, e.payload_json
		FROM events_fts
		JOIN events e ON e.rowid = events_fts.rowid
		JOIN sessions s ON s.id = e.session_id
		WHERE events_fts MATCH ?
		ORDER BY e.ts DESC LIMIT 10
	`, "rate")
	if err != nil {
		t.Fatal("query no snippet:", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sid, agent, typ, ts string
		var payload []byte
		rows.Scan(&id, &sid, &agent, &typ, &ts, &payload)
		fmt.Printf("id=%s type=%s payload=%s\n", id, typ, payload)
	}
	if err := rows.Err(); err != nil {
		t.Fatal("rows err:", err)
	}

	// Try snippet with a workaround: use events_fts as a secondary index,
	// and use highlight on a subquery that selects payload_json as payload
	// Actually, we can use snippet() if we query the FTS as a subquery of itself.
	// Or we could create a view...
	// Simplest: just use e.payload_json and do our own substring extraction.
	fmt.Println("--- testing snippet with alias ---")
	rows2, err := d.Query(`
		WITH src AS (SELECT rowid, payload_json AS payload FROM events)
		SELECT snippet(events_fts, 3, '<<', '>>', '…', 30)
		FROM events_fts
		WHERE events_fts MATCH ?
	`, "rate")
	if err != nil {
		t.Logf("snippet with CTE failed: %v", err)
	} else {
		defer rows2.Close()
		for rows2.Next() {
			var snip string
			rows2.Scan(&snip)
			fmt.Println("snippet:", snip)
		}
	}
}
