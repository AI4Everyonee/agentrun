package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/jeevan/agentrun/internal/agent"
	"github.com/jeevan/agentrun/internal/config"
	"github.com/jeevan/agentrun/internal/db"
	"github.com/jeevan/agentrun/internal/gitmeta"
	"github.com/jeevan/agentrun/internal/pty"
	"github.com/jeevan/agentrun/internal/recorder"
)

// runAgent is the shared wrapper logic for all agent subcommands.
//
//   - agentName: "claude" or "codex" (stored in sessions.agent)
//   - args:      passthrough args for the child process
//
// It NEVER returns nil on success — it calls os.Exit(childExitCode) so the
// wrapper is transparent. It returns a non-nil error only for setup failures
// (agent not on PATH, DB open failure, etc.).
func runAgent(agentName string, args []string) error {
	// 1. Resolve agent binary.
	binPath, err := agent.Resolve(agentName)
	if err != nil {
		if errors.Is(err, agent.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "agentrun: %s not found on PATH\n", agentName)
			os.Exit(127)
		}
		return err
	}

	// 2. Detect version (best-effort; empty string is fine).
	version := agent.Version(binPath)

	// 3. Load config (resolves DBDir, DBPath, ArtifactsDir).
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// 4. Open DB (creates DB dir + applies schema).
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	defer d.Close()

	// 5. Gather git metadata (best-effort; partial results are fine).
	cwd, _ := os.Getwd()
	snap := gitmeta.Capture(cwd)

	// 6. Start recorder: inserts session row, artifact rows, starts writer goroutine,
	//    and emits session.started synchronously.
	rec, err := recorder.Start(d, recorder.StartOpts{
		Agent:          agentName,
		AgentVersion:   version,
		Cwd:            cwd,
		RepoRoot:       snap.RepoRoot,
		Branch:         snap.Branch,
		StartCommitSHA: snap.HeadSHA,
		ArtifactsDir:   cfg.ArtifactsDir,
		Redactor:       recorder.NoopRedactor{},
	})
	if err != nil {
		return fmt.Errorf("recorder start: %w", err)
	}

	// 7. Open artifact files for the tee writers.
	//    recorder.Start already created the files; we open them for appending.
	ptyFile, err := os.OpenFile(rec.PtyArtifactPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		rec.Close("", 1)
		return fmt.Errorf("open pty artifact: %w", err)
	}
	defer ptyFile.Close()

	stdinFile, err := os.OpenFile(rec.StdinArtifactPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		ptyFile.Close()
		rec.Close("", 1)
		return fmt.Errorf("open stdin artifact: %w", err)
	}
	defer stdinFile.Close()

	// 8. Chunkers tee bytes from the PTY copy loops into recorder events.
	stdoutChunker := recorder.NewChunker(rec, "pty", "terminal.output")
	stdinChunker := recorder.NewChunker(rec, "pty", "terminal.stdin")
	// These defers are safety nets for early-return paths only.
	// On the normal path we call Close() explicitly before os.Exit.
	defer stdoutChunker.Close()
	defer stdinChunker.Close()

	// 9. Build the child command.
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), "AGENTRUN_SESSION_ID="+rec.SessionID())
	cmd.Dir = cwd

	// 10. Tee writers: each byte goes to the artifact file AND the chunker.
	stdoutTee := io.MultiWriter(ptyFile, stdoutChunker)
	stdinTee := io.MultiWriter(stdinFile, stdinChunker)

	// 11. Start PTY: puts host terminal into raw mode, launches child, wires copy loops.
	handle, err := pty.Start(cmd, stdinTee, stdoutTee)
	if err != nil {
		rec.Close("", 1)
		return fmt.Errorf("pty start: %w", err)
	}
	// defer Close is a safety net for panics; the normal path calls it explicitly below.
	defer handle.Close()

	// 12. Update the session row with the child's real PID.
	if cmd.Process != nil {
		_ = rec.UpdatePID(cmd.Process.Pid)
	}

	// 13. Forward SIGINT/SIGTERM to the child so the wrapper is transparent.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for sig := range sigCh {
			_ = handle.Signal(sig)
		}
	}()
	defer signal.Stop(sigCh)
	defer close(sigCh)

	// 14-17. Wait, flush, finalize, and exit.
	//
	// NOTE: os.Exit below bypasses all remaining defers, so we handle the happy
	// path explicitly here. The defers above are only safety nets for panics and
	// unexpected early returns.
	exitCode := handle.Wait()

	// 15. Flush chunkers BEFORE closing recorder so the last bytes get emitted.
	stdoutChunker.Flush()
	stdinChunker.Flush()

	// 16. Capture end-of-session git commit SHA, then finalize the recorder.
	endSHA := gitmeta.HeadCommit(cwd)
	if err := rec.Close(endSHA, exitCode); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: recorder close warning: %v\n", err)
	}

	// 17. Restore terminal before exiting so the user's shell is not left broken.
	handle.Close()

	// 17. Exit with the child's code so the wrapper is transparent.
	os.Exit(exitCode)
	return nil // unreachable
}
