package cli

import (
	"errors"
	"fmt"
)

// ErrUsage indicates the user invoked the CLI incorrectly. main() maps it to exit 2.
var ErrUsage = errors.New("usage")

// Run executes the CLI with args stripped of argv[0]. It is the single entrypoint
// from main(). Handlers that wrap a child agent call os.Exit themselves so the
// wrapper is transparent; Run only returns to main() for setup-time errors.
func Run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "claude":
		return runClaude(args[1:])
	case "codex":
		return runCodex(args[1:])
	case "sessions":
		return runSessions(args[1:])
	case "show":
		return runShow(args[1:])
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command: %q (try 'agentrun help')", args[0])
	}
}

func usage() error {
	fmt.Println(`agentrun — record local agent sessions

Usage:
  agentrun claude [args...]    Run the Claude Code CLI under recording
  agentrun codex  [args...]    Run the Codex CLI under recording
  agentrun sessions            List recorded sessions
  agentrun show <session_id>   Show summary for a session
  agentrun help                Show this help`)
	return ErrUsage
}

// Subcommand handlers — populated in task #8.
func runClaude(args []string) error   { return errors.New("not yet implemented") }
func runCodex(args []string) error    { return errors.New("not yet implemented") }
func runSessions(args []string) error { return errors.New("not yet implemented") }
func runShow(args []string) error     { return errors.New("not yet implemented") }
