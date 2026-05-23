package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AI4Everyonee/agentrun/internal/collector"
)

// runCollector implements `agentrun collector [start|stop|status]`.
func runCollector(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "agentrun collector: usage: agentrun collector <start|stop|status>")
		return ErrUsage
	}
	switch args[0] {
	case "start":
		return runCollectorStart(args[1:])
	case "stop":
		return runCollectorStop(args[1:])
	case "status":
		return runCollectorStatus(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "agentrun collector: unknown subcommand %q\n", args[0])
		return ErrUsage
	}
}

// runCollectorStart implements `agentrun collector start [--socket <path>] [--foreground]`.
func runCollectorStart(args []string) error {
	fs := flag.NewFlagSet("collector-start", flag.ContinueOnError)
	socketPath := fs.String("socket", collector.DefaultSocketPath(), "Unix socket path")
	foreground := fs.Bool("foreground", false, "Run in foreground instead of daemonizing")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	pidPath := collectorPIDPath()
	dbPath, err := resolveDBPath()
	if err != nil {
		return fmt.Errorf("collector start: %w", err)
	}

	// Check if already running.
	if pid, err := readPIDFile(pidPath); err == nil && pid > 0 {
		if processAlive(pid) {
			return fmt.Errorf("collector: already running (pid %d); stop it first", pid)
		}
		// Stale PID file — remove it.
		_ = os.Remove(pidPath)
	}

	if *foreground {
		return runCollectorForeground(*socketPath, dbPath, pidPath)
	}

	// Daemonize: re-exec ourselves with --foreground.
	return daemonizeCollector(*socketPath, pidPath)
}

// runCollectorForeground runs the collector server in the current process.
func runCollectorForeground(socketPath, dbPath, pidPath string) error {
	srv, err := collector.New(socketPath, dbPath)
	if err != nil {
		return fmt.Errorf("collector: create server: %w", err)
	}
	if err := srv.Start(); err != nil {
		return fmt.Errorf("collector: start: %w", err)
	}

	// Write our own PID.
	pid := os.Getpid()
	if err := writePIDFile(pidPath, pid); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: write pid file: %v\n", err)
	}

	fmt.Printf("agentrun collector: listening on %s (pid %d)\n", socketPath, pid)

	// Wait for SIGTERM or SIGINT.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	<-sigCh

	fmt.Fprintln(os.Stderr, "agentrun collector: shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("collector: shutdown: %w", err)
	}
	_ = os.Remove(pidPath)
	fmt.Fprintln(os.Stderr, "agentrun collector: done")
	return nil
}

// daemonizeCollector forks a background process running `agentrun collector start --foreground`.
func daemonizeCollector(socketPath, pidPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("collector: resolve executable: %w", err)
	}

	logPath := strings.TrimSuffix(pidPath, ".pid") + ".log"
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		// Non-fatal — use /dev/null.
		logFile, _ = os.Open(os.DevNull)
	}
	defer logFile.Close()

	proc, err := os.StartProcess(exe, []string{
		exe, "collector", "start",
		"--foreground",
		"--socket", socketPath,
	}, &os.ProcAttr{
		Files: []*os.File{
			nil,      // stdin → closed
			logFile,  // stdout → log file
			logFile,  // stderr → log file
		},
		Sys: &syscall.SysProcAttr{
			Setsid: true, // detach from terminal
		},
	})
	if err != nil {
		return fmt.Errorf("collector: fork background process: %w", err)
	}

	// Release the child so it runs independently.
	if err := proc.Release(); err != nil {
		return fmt.Errorf("collector: release child: %w", err)
	}

	fmt.Printf("agentrun collector: started in background (pid %d), log: %s\n", proc.Pid, logPath)

	// Write PID file from the parent so that `collector status` works immediately.
	if err := writePIDFile(pidPath, proc.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun collector: write pid file: %v\n", err)
	}

	return nil
}

// runCollectorStop implements `agentrun collector stop [--socket <path>]`.
func runCollectorStop(args []string) error {
	fs := flag.NewFlagSet("collector-stop", flag.ContinueOnError)
	_ = fs.String("socket", collector.DefaultSocketPath(), "Unix socket path (unused; kept for symmetry)")
	if err := fs.Parse(args); err != nil {
		return ErrUsage
	}

	pidPath := collectorPIDPath()

	pid, err := readPIDFile(pidPath)
	if err != nil {
		fmt.Println("agentrun collector: not running")
		return nil
	}

	if !processAlive(pid) {
		fmt.Println("agentrun collector: not running (stale pid file)")
		_ = os.Remove(pidPath)
		return nil
	}

	// Send SIGTERM.
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("collector stop: find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("collector stop: send SIGTERM to %d: %w", pid, err)
	}

	// Wait up to 5s for the process to exit.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if processAlive(pid) {
		return fmt.Errorf("collector stop: process %d did not exit within 5s", pid)
	}

	_ = os.Remove(pidPath)
	_ = os.Remove(collector.DefaultSocketPath())
	fmt.Printf("agentrun collector: stopped (pid %d)\n", pid)
	return nil
}

// runCollectorStatus implements `agentrun collector status`.
func runCollectorStatus(args []string) error {
	_ = args // no flags
	pidPath := collectorPIDPath()
	socketPath := collector.DefaultSocketPath()

	pid, err := readPIDFile(pidPath)
	if err != nil || pid <= 0 {
		fmt.Printf("agentrun collector: not running\n  socket: %s\n  pid file: %s\n", socketPath, pidPath)
		return nil
	}

	alive := processAlive(pid)
	status := "running"
	if !alive {
		status = "dead (stale pid file)"
	}

	fmt.Printf("agentrun collector: %s\n  pid: %d\n  socket: %s\n  pid file: %s\n",
		status, pid, socketPath, pidPath)
	return nil
}

// collectorPIDPath returns the PID file path for the collector.
func collectorPIDPath() string {
	return collector.DefaultPIDPath()
}

// readPIDFile reads a PID from path. Returns error if path doesn't exist or content is invalid.
func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file %s: %w", path, err)
	}
	return pid, nil
}

// writePIDFile writes pid to path (creating parent dirs as needed).
func writePIDFile(path string, pid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// processAlive returns true if a process with the given pid exists and is alive.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
