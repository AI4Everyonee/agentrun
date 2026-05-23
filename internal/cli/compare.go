package cli

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jeevan/agentrun/internal/db"
)

// runCompare shows a side-by-side event timeline of two sessions.
// Usage: agentrun compare <id_a> <id_b>
func runCompare(args []string) error {
	if len(args) != 2 {
		return ErrUsage
	}
	idA, idB := args[0], args[1]

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	sessA, err := db.GetSession(d, idA)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %q not found", idA)
		}
		return err
	}
	sessB, err := db.GetSession(d, idB)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("session %q not found", idB)
		}
		return err
	}

	eventsA, err := db.FetchSessionEvents(d, idA)
	if err != nil {
		return fmt.Errorf("compare: fetch events A: %w", err)
	}
	eventsB, err := db.FetchSessionEvents(d, idB)
	if err != nil {
		return fmt.Errorf("compare: fetch events B: %w", err)
	}

	fmt.Printf("agentrun compare %s %s\n\n", truncate(idA, 40), truncate(idB, 40))

	// Print session headers.
	repoA := sessionRepo(sessA)
	repoB := sessionRepo(sessB)
	timeA := sessA.StartedAt.Local().Format("15:04")
	timeB := sessB.StartedAt.Local().Format("15:04")

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  Session A:\t%s\t%s\t%s\t%s\n", truncate(idA, 30), sessA.Agent, truncate(repoA, 25), timeA)
	fmt.Fprintf(tw, "  Session B:\t%s\t%s\t%s\t%s\n", truncate(idB, 30), sessB.Agent, truncate(repoB, 25), timeB)
	tw.Flush()
	fmt.Println()

	// Side-by-side event table.
	lenA := len(eventsA)
	lenB := len(eventsB)
	maxLen := lenA
	if lenB > maxLen {
		maxLen = lenB
	}

	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  #\tA\tB\n")
	for i := 0; i < maxLen; i++ {
		var sumA, sumB string
		if i < lenA {
			sumA = eventSummary(eventsA[i])
		} else {
			sumA = "—"
		}
		if i < lenB {
			sumB = eventSummary(eventsB[i])
		} else {
			sumB = "—"
		}
		fmt.Fprintf(tw, "  %d\t%s\t%s\n", i+1, sumA, sumB)
	}
	tw.Flush()

	return nil
}

// sessionRepo returns the repo root of a session, falling back to cwd.
func sessionRepo(s db.SessionRow) string {
	if s.RepoRoot.Valid && s.RepoRoot.String != "" {
		return s.RepoRoot.String
	}
	return s.Cwd
}

// eventSummary produces a short human-readable label for an event row.
// For tool events it includes the tool_name and the first token of any command.
func eventSummary(e db.EventRow) string {
	label := e.Type

	// Try to extract tool_name and input from payload.
	var payload map[string]interface{}
	if err := json.Unmarshal(e.PayloadJSON, &payload); err == nil {
		toolName, _ := payload["tool_name"].(string)
		if toolName == "" {
			toolName, _ = payload["name"].(string)
		}

		if toolName != "" {
			label = e.Type + "  (" + toolName

			// Try to get a first token of the command from input fields.
			if input, ok := payload["input"].(map[string]interface{}); ok {
				for _, key := range []string{"command", "cmd", "prompt", "path", "file_path"} {
					if val, ok := input[key].(string); ok && val != "" {
						tok := firstToken(val)
						if tok != "" {
							label += ": " + tok
						}
						break
					}
				}
			}

			label += ")"
		}
	}

	return label
}

// firstToken returns the first whitespace-separated token of s, truncated to 30 chars.
func firstToken(s string) string {
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return ""
	}
	tok := fields[0]
	if len(tok) > 30 {
		tok = tok[:30]
	}
	return tok
}

// Ensure time is imported (used indirectly via db.SessionRow).
var _ = time.Now
