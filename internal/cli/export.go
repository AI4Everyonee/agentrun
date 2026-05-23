package cli

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// exportHeader is the first line written to the JSONL output.
type exportHeader struct {
	AgentrunExportVersion string          `json:"agentrun_export_version"`
	Session               db.SessionRow   `json:"session"`
	Artifacts             []db.ArtifactRow `json:"artifacts"`
}

// exportEvent is the shape of each event line in the JSONL output.
type exportEvent struct {
	ID               string          `json:"id"`
	SessionID        string          `json:"session_id"`
	Sequence         int64           `json:"sequence"`
	Ts               string          `json:"ts"`
	Source           string          `json:"source"`
	Type             string          `json:"type"`
	Payload          json.RawMessage `json:"payload"`
	RedactionVersion string          `json:"redaction_version,omitempty"`
}

// runExport exports a session as JSONL to stdout or a file.
// Usage: agentrun export <session_id> [--format jsonl] [--output <file>]
func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	format := fs.String("format", "jsonl", "export format (only jsonl supported)")
	output := fs.String("output", "", "output file path (default: stdout)")

	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	remaining := fs.Args()
	if len(remaining) != 1 {
		return ErrUsage
	}
	sid := remaining[0]

	if *format != "jsonl" {
		return fmt.Errorf("unsupported format %q (only jsonl is supported)", *format)
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

	s, err := db.GetSession(d, sid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %q not found", sid)
		}
		return err
	}

	artifacts, err := db.ListArtifacts(d, sid)
	if err != nil {
		return err
	}
	if artifacts == nil {
		artifacts = []db.ArtifactRow{}
	}

	events, err := db.FetchSessionEvents(d, sid)
	if err != nil {
		return err
	}

	// Determine output writer.
	var w io.Writer = os.Stdout
	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			return fmt.Errorf("export: create output file: %w", err)
		}
		defer f.Close()
		w = f
	}

	bw := bufio.NewWriter(w)

	// Write header line.
	header := exportHeader{
		AgentrunExportVersion: "1",
		Session:               s,
		Artifacts:             artifacts,
	}
	hBytes, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("export: marshal header: %w", err)
	}
	if _, err := bw.Write(hBytes); err != nil {
		return fmt.Errorf("export: write header: %w", err)
	}
	if err := bw.WriteByte('\n'); err != nil {
		return fmt.Errorf("export: write header newline: %w", err)
	}

	// Write one event line per event.
	for _, e := range events {
		ev := exportEvent{
			ID:        e.ID,
			SessionID: e.SessionID,
			Sequence:  e.Sequence,
			Ts:        e.Ts.UTC().Format(time.RFC3339Nano),
			Source:    e.Source,
			Type:      e.Type,
			Payload:   json.RawMessage(e.PayloadJSON),
		}
		if e.RedactionVersion.Valid {
			ev.RedactionVersion = e.RedactionVersion.String
		}

		line, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("export: marshal event %s: %w", e.ID, err)
		}
		if _, err := bw.Write(line); err != nil {
			return fmt.Errorf("export: write event: %w", err)
		}
		if err := bw.WriteByte('\n'); err != nil {
			return fmt.Errorf("export: write event newline: %w", err)
		}
	}

	return bw.Flush()
}
