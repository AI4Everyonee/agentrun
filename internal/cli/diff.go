package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"

	"golang.org/x/term"

	"github.com/jeevan/agentrun/internal/db"
)

// runDiff pipes the captured git_diff artifact through $PAGER (default less -R).
// If no git_diff artifact exists, prints a message and exits cleanly.
// Usage: agentrun diff <session_id>
func runDiff(args []string) error {
	if len(args) != 1 {
		return ErrUsage
	}
	sid := args[0]

	dbPath, err := resolveDBPath()
	if err != nil {
		return err
	}

	d, err := db.Open(dbPath)
	if err != nil {
		return err
	}
	defer d.Close()

	artifacts, err := db.ListArtifacts(d, sid)
	if err != nil {
		return fmt.Errorf("diff: list artifacts: %w", err)
	}

	// Find a git_diff artifact.
	var gitDiffPath string
	for _, a := range artifacts {
		if a.Kind == "git_diff" && a.Path.Valid && a.Path.String != "" {
			gitDiffPath = a.Path.String
			break
		}
	}

	if gitDiffPath == "" {
		fmt.Printf("no git diff captured for session %s\n", sid)
		return nil
	}

	f, err := os.Open(gitDiffPath)
	if err != nil {
		return fmt.Errorf("diff: open artifact: %w", err)
	}
	defer f.Close()

	// If stdout is not a TTY and PAGER is not explicitly set, just copy.
	pager := os.Getenv("PAGER")
	if pager == "" && !term.IsTerminal(int(os.Stdout.Fd())) {
		_, err = io.Copy(os.Stdout, f)
		return err
	}

	if pager == "" {
		pager = "less -R"
	}

	cmd := exec.Command("sh", "-c", pager)
	cmd.Stdin = f
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
