package cli

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// runTag annotates a session by inserting a synthetic event of type user.tag.
// Usage: agentrun tag <session_id> <tag>
func runTag(args []string) error {
	if len(args) != 2 {
		return ErrUsage
	}
	sid, tag := args[0], args[1]

	if tag == "" {
		return ErrUsage
	}

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	// Verify the session exists.
	if _, err := db.GetSession(d, sid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %q not found", sid)
		}
		return fmt.Errorf("tag: get session: %w", err)
	}

	now := time.Now().UTC()
	payload := []byte(fmt.Sprintf(`{"tag":%q,"applied_at":%q}`, tag, now.Format(time.RFC3339Nano)))

	evtID, err := db.InsertEventWithAutoSeq(d, sid, now, "user", "user.tag", payload, "noop-1")
	if err != nil {
		return fmt.Errorf("tag: insert event: %w", err)
	}

	// Write the tag into events.tags column for the row just inserted.
	if _, err := d.Exec(`UPDATE events SET tags = ? WHERE id = ?`, tag, evtID); err != nil {
		return fmt.Errorf("tag: update tags column: %w", err)
	}

	fmt.Printf("agentrun: tagged session %s with %q (event %s)\n", sid, tag, evtID)
	return nil
}
