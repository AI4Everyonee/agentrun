//go:build !windows

// Package pty wraps a child process inside a pseudo-terminal so a TUI like
// Claude Code or Codex renders correctly while every byte is teed into the
// recorder.
//
// IMPORTANT: PTY merges stdout and stderr into a single byte stream — this is
// a fundamental property of pseudo-terminals, not a design choice of this
// recorder. Events sourced from this stream are typed terminal.output, not
// separate terminal.stdout / terminal.stderr. To distinguish the two, Phase 2+
// must rely on hooks or invoke the child outside a PTY (which would break the
// TUI). Stdin is captured at the keystroke level because the host terminal is
// in raw mode; high-level prompt extraction is Phase 2's job.
package pty

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	creackpty "github.com/creack/pty"
	"golang.org/x/term"
)

// Handle is the running child + PTY pair.
type Handle struct {
	ptmx        *os.File
	cmd         *exec.Cmd
	restoreTerm func() error
	stopSIGWINCH func()

	waitOnce sync.Once
	exitCode int

	closeOnce sync.Once
}

// Start launches cmd attached to a new PTY.
// stdinTee, stdoutTee may be nil; if non-nil, copied bytes are also written there.
// The host terminal is put into raw mode; restore is wired into Handle.Close.
//
// If cmd is nil or has no Path set, Start returns an error immediately without
// touching the terminal. This guards against leaving the terminal in raw mode
// on a setup error.
//
// Risk R3 mitigation: creackpty.InheritSize is called once unconditionally
// immediately after creackpty.Start returns, before copy goroutines start.
// This prevents a stale PTY size if the host terminal was resized between
// process fork and SIGWINCH goroutine startup.
//
// Risk R1 mitigation: if term.MakeRaw succeeds but creackpty.Start fails,
// term.Restore is called before returning the error so the caller's terminal
// is not left broken.
func Start(cmd *exec.Cmd, stdinTee, stdoutTee io.Writer) (*Handle, error) {
	if cmd == nil || cmd.Path == "" {
		return nil, errors.New("pty: cmd has no Path")
	}

	// Put host terminal into raw mode. This may fail in test environments
	// where os.Stdin is a pipe rather than a real TTY. That is acceptable —
	// we treat raw mode as best-effort and proceed either way.
	oldState, rawErr := term.MakeRaw(int(os.Stdin.Fd()))

	var restoreTerm func() error
	if rawErr != nil || oldState == nil {
		// Not a TTY (e.g. running under go test), or raw mode failed.
		// Provide a no-op restore so the rest of the code is uniform.
		restoreTerm = func() error { return nil }
		oldState = nil
	} else {
		restoreTerm = func() error {
			return term.Restore(int(os.Stdin.Fd()), oldState)
		}
	}

	// Start the child process inside a new PTY.
	ptmx, err := creackpty.Start(cmd)
	if err != nil {
		// Risk R1: restore terminal before returning error.
		if oldState != nil {
			_ = term.Restore(int(os.Stdin.Fd()), oldState)
		}
		return nil, err
	}

	// Risk R3: inherit host terminal size once immediately so the child PTY
	// starts with the correct dimensions even if a SIGWINCH arrived between
	// fork and goroutine startup.
	_ = creackpty.InheritSize(os.Stdin, ptmx)

	// SIGWINCH goroutine: forward terminal resize events to the child PTY.
	sigCh := make(chan os.Signal, 1)
	quit := make(chan struct{})
	signal.Notify(sigCh, syscall.SIGWINCH)

	go func() {
		for {
			select {
			case <-sigCh:
				_ = creackpty.InheritSize(os.Stdin, ptmx)
			case <-quit:
				return
			}
		}
	}()

	stopSIGWINCH := func() {
		signal.Stop(sigCh)
		// close quit only once; the goroutine exits on the next select.
		select {
		case <-quit:
			// already closed
		default:
			close(quit)
		}
	}

	// stdin -> child PTY (+ optional tee).
	go func() {
		if stdinTee != nil {
			_, _ = io.Copy(io.MultiWriter(ptmx, stdinTee), os.Stdin)
		} else {
			_, _ = io.Copy(ptmx, os.Stdin)
		}
	}()

	// child PTY -> stdout (+ optional tee).
	go func() {
		if stdoutTee != nil {
			_, _ = io.Copy(io.MultiWriter(os.Stdout, stdoutTee), ptmx)
		} else {
			_, _ = io.Copy(os.Stdout, ptmx)
		}
	}()

	return &Handle{
		ptmx:         ptmx,
		cmd:          cmd,
		restoreTerm:  restoreTerm,
		stopSIGWINCH: stopSIGWINCH,
	}, nil
}

// Wait blocks until the child exits and returns its exit code.
// On a clean exit, returns cmd.ProcessState.ExitCode().
// On a signal-induced termination, returns 128 + signal number (POSIX convention).
// On any other error from cmd.Wait, returns 1.
// Safe to call multiple times — subsequent calls return the cached exit code.
func (h *Handle) Wait() int {
	h.waitOnce.Do(func() {
		err := h.cmd.Wait()
		if err == nil {
			h.exitCode = 0
			return
		}
		ps := h.cmd.ProcessState
		if ps == nil {
			h.exitCode = 1
			return
		}
		if ps.Exited() {
			h.exitCode = ps.ExitCode()
			return
		}
		// Signal-killed: compute 128 + signal number per POSIX convention.
		ws, ok := ps.Sys().(syscall.WaitStatus)
		if ok {
			h.exitCode = 128 + int(ws.Signal())
			return
		}
		h.exitCode = 1
	})
	return h.exitCode
}

// Close restores the host terminal mode and stops the SIGWINCH goroutine.
// Idempotent — safe to call multiple times.
// ptmx is closed first; restoreTerm runs even if ptmx.Close errors.
func (h *Handle) Close() error {
	var closeErr error
	h.closeOnce.Do(func() {
		// Close the PTY master. This will cause the stdin/stdout copy goroutines
		// to return (their source/destination has closed).
		if h.ptmx != nil {
			closeErr = h.ptmx.Close()
		}
		// Restore terminal mode regardless of the ptmx close result.
		if h.restoreTerm != nil {
			_ = h.restoreTerm()
		}
		// Stop the SIGWINCH goroutine.
		if h.stopSIGWINCH != nil {
			h.stopSIGWINCH()
		}
	})
	return closeErr
}

// Signal forwards sig to the child process.
// Returns an error if the child process has not been started.
func (h *Handle) Signal(sig os.Signal) error {
	if h.cmd == nil || h.cmd.Process == nil {
		return errors.New("pty: process not started")
	}
	return h.cmd.Process.Signal(sig)
}
