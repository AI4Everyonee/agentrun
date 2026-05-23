package db

import (
	"database/sql"
	"fmt"
	"time"
)

// DeleteSessionsOlderThan deletes sessions whose started_at is before cutoff
// and whose status in session_summary is not 'running' (to avoid deleting
// in-flight sessions). Returns the IDs of the deleted sessions for on-disk cleanup.
// CASCADE on events, artifacts, and session_summary means child rows are removed
// automatically when the sessions row is deleted.
func DeleteSessionsOlderThan(d *sql.DB, cutoff time.Time) ([]string, error) {
	cutoffStr := fmtTime(cutoff)

	// Collect the IDs first so we can return them to the caller for disk cleanup.
	const selectQ = `
		SELECT s.id
		FROM sessions s
		JOIN session_summary ss ON ss.session_id = s.id
		WHERE s.started_at < ?
		  AND ss.status != 'running'`

	rows, err := d.Query(selectQ, cutoffStr)
	if err != nil {
		return nil, fmt.Errorf("DeleteSessionsOlderThan: select IDs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("DeleteSessionsOlderThan: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("DeleteSessionsOlderThan: rows.Err: %w", err)
	}
	rows.Close()

	if len(ids) == 0 {
		return nil, nil
	}

	// Delete the sessions rows. CASCADE removes events, artifacts, session_summary.
	const deleteQ = `
		DELETE FROM sessions
		WHERE id IN (
			SELECT s.id
			FROM sessions s
			JOIN session_summary ss ON ss.session_id = s.id
			WHERE s.started_at < ?
			  AND ss.status != 'running'
		)`

	if _, err := d.Exec(deleteQ, cutoffStr); err != nil {
		return nil, fmt.Errorf("DeleteSessionsOlderThan: delete: %w", err)
	}

	return ids, nil
}

// TailEventsSince returns up to limit events whose ts is strictly after since,
// ordered by ts ASC, sequence ASC. Used by `agentrun watch` to implement polling.
func TailEventsSince(d *sql.DB, since time.Time, limit int) ([]EventRow, error) {
	const q = `
		SELECT id, session_id, sequence, ts, source, type, payload_json, redaction_version
		FROM events
		WHERE ts > ?
		ORDER BY ts ASC, sequence ASC
		LIMIT ?`

	sinceStr := fmtTime(since)
	rows, err := d.Query(q, sinceStr, limit)
	if err != nil {
		return nil, fmt.Errorf("TailEventsSince: query: %w", err)
	}
	defer rows.Close()

	var result []EventRow
	for rows.Next() {
		var e EventRow
		var tsStr string
		var redactVer sql.NullString
		if err := rows.Scan(
			&e.ID, &e.SessionID, &e.Sequence, &tsStr,
			&e.Source, &e.Type, &e.PayloadJSON, &redactVer,
		); err != nil {
			return nil, fmt.Errorf("TailEventsSince: scan: %w", err)
		}
		e.Ts, err = parseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("TailEventsSince: parse ts: %w", err)
		}
		e.RedactionVersion = redactVer
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("TailEventsSince: rows: %w", err)
	}
	return result, nil
}
