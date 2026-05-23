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
	case "export":
		return runExport(args[1:])
	case "finalize-idle":
		return runFinalizeIdle(args[1:])
	case "hook":
		return runHook(args[1:])
	case "install":
		return runInstall(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	case "gc":
		return runGC(args[1:])
	case "watch":
		return runWatch(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "stats":
		return runStats(args[1:])
	case "diff":
		return runDiff(args[1:])
	case "search":
		return runSearch(args[1:])
	case "replay":
		return runReplay(args[1:])
	case "compare":
		return runCompare(args[1:])
	case "tag":
		return runTag(args[1:])
	case "collector":
		return runCollector(args[1:])
	case "help", "-h", "--help":
		return usage()
	default:
		return fmt.Errorf("unknown command: %q (try 'agentrun help')", args[0])
	}
}

func usage() error {
	fmt.Println(`agentrun — record local agent sessions

Usage:
  agentrun install                Register agentrun hooks globally for claude + codex
  agentrun uninstall              Remove agentrun hooks from ~/.claude and ~/.codex
  agentrun claude [args...]       Opt-in: run Claude under recording WITH PTY capture
  agentrun codex  [args...]       Opt-in: run Codex under recording WITH PTY capture
  agentrun sessions               List recorded sessions
  agentrun show <session_id>      Show summary for a session
  agentrun export <session_id>    Export session as JSONL (1 line per event)
  agentrun diff <session_id>      Pipe captured git.diff through $PAGER
  agentrun search <query>         Full-text search across event payloads + prompts
  agentrun stats [--repo <dir>]   Per-agent / per-repo usage rollup
  agentrun compare <id_a> <id_b>  Side-by-side event timeline of two sessions
  agentrun replay <session_id>    Re-emit captured PTY bytes (wrapped sessions only)
  agentrun watch                  Tail events live across all sessions
  agentrun tag <session_id> <tag> Annotate a session with a free-text tag
  agentrun finalize-idle          Sweep stale 'running' sessions (e.g. Codex never-ends)
  agentrun gc [--older-than 30d]  Delete old sessions + artifacts
  agentrun doctor                 Verify installation health
  agentrun collector [start|stop] Long-lived hook receiver via Unix socket
  agentrun hook <agent> <event>   Internal: invoked by claude/codex hooks (do not call directly)
  agentrun help                   Show this help`)
	return ErrUsage
}

// Subcommand handlers are implemented in their respective files:
//   claude.go    — runClaude
//   codex.go     — runCodex
//   sessions.go  — runSessions
//   show.go      — runShow
