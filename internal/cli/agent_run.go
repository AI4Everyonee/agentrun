package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/AI4Everyonee/agentrun/internal/agent"
	"github.com/AI4Everyonee/agentrun/internal/config"
	"github.com/AI4Everyonee/agentrun/internal/db"
	"github.com/AI4Everyonee/agentrun/internal/fswatcher"
	"github.com/AI4Everyonee/agentrun/internal/gitmeta"
	"github.com/AI4Everyonee/agentrun/internal/pty"
	"github.com/AI4Everyonee/agentrun/internal/recorder"
	"github.com/AI4Everyonee/agentrun/internal/redact"
	"github.com/AI4Everyonee/agentrun/internal/userident"
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
		UserName:       userident.Detect(),
		Cwd:            cwd,
		RepoRoot:       snap.RepoRoot,
		Branch:         snap.Branch,
		StartCommitSHA: snap.HeadSHA,
		ArtifactsDir:   cfg.ArtifactsDir,
		Redactor:       redact.Default(),
	})
	if err != nil {
		return fmt.Errorf("recorder start: %w", err)
	}

	// 6b. The wrapper used to inject per-session hook config (--settings for
	//     Claude, -c for Codex). That's gone — global install via
	//     `agentrun install` now writes the hook config to ~/.claude/settings.json
	//     and ~/.codex/config.toml, so hooks fire for every claude/codex
	//     invocation on the laptop, wrapped or not.
	//
	//     The wrapper's remaining job for hooks is to inject AGENTRUN_SESSION_ID
	//     and AGENTRUN_DB_PATH into the child env. The global hook command sees
	//     these and attributes events to the wrapper's session row (with PTY
	//     artifacts) instead of synthesizing a native session ID.

	// 6c. Start the filesystem watcher (wrapper-only — native sessions don't
	//     get this since there's no long-lived process to host fsnotify).
	//     Failure here is non-fatal; we just skip fs events for this session.
	var fsw *fswatcher.Watcher
	if w, fsErr := fswatcher.New(cwd, rec); fsErr == nil {
		if startErr := w.Start(); startErr == nil {
			fsw = w
		} else {
			fmt.Fprintf(os.Stderr, "agentrun: fswatcher start failed (continuing without fs events): %v\n", startErr)
			_ = w.Close()
		}
	} else {
		fmt.Fprintf(os.Stderr, "agentrun: fswatcher init failed (continuing without fs events): %v\n", fsErr)
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

	// 9. Build the child command. No flag injection — hooks are now global
	//    (see step 6b). Args pass through verbatim.
	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(),
		"AGENTRUN_SESSION_ID="+rec.SessionID(),
		"AGENTRUN_DB_PATH="+cfg.DBPath,
	)
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

	// 15b. Stop the filesystem watcher (drains pending events into the recorder).
	if fsw != nil {
		if err := fsw.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "agentrun: fswatcher close warning: %v\n", err)
		}
	}

	// 16. Capture end-of-session git commit SHA, then finalize the recorder.
	endSHA := gitmeta.HeadCommit(cwd)
	if err := rec.Close(endSHA, exitCode); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: recorder close warning: %v\n", err)
	}

	// 17. Restore terminal before exiting so the user's shell is not left broken.
	handle.Close()

	// 17b. Fire `agentrun summarize <sid>` detached. The child survives our
	//      exit (Setsid) and writes the OpenAI summary asynchronously. No
	//      key set → scheduleSummary is a no-op.
	scheduleSummary(rec.SessionID())

	// 18. Exit with the child's code so the wrapper is transparent.
	os.Exit(exitCode)
	return nil // unreachable
}

// resolveSelfPath returns the absolute path to the running agentrun binary.
// Tries os.Executable() first (with EvalSymlinks), falls back to exec.LookPath("agentrun").
// Returns "" if both fail — caller must treat that as "do not register hooks".
func resolveSelfPath() string {
	if p, err := os.Executable(); err == nil && p != "" {
		if abs, err := filepath.EvalSymlinks(p); err == nil {
			return abs
		}
		return p
	}
	if p, err := exec.LookPath("agentrun"); err == nil {
		return p
	}
	return ""
}
