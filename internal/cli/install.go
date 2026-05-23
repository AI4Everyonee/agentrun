package cli

import (
	"fmt"
	"os"

	"github.com/jeevan/agentrun/internal/install"
)

// runInstall handles `agentrun install`. It writes the global hook config to
// ~/.claude/settings.json and ~/.codex/config.toml so every claude/codex
// invocation on the laptop records events into ~/.agentrun/agentrun.db.
func runInstall(args []string) error {
	if len(args) > 0 {
		return ErrUsage
	}

	binPath := resolveSelfPath()
	if binPath == "" {
		return fmt.Errorf("cannot determine agentrun binary path; install requires the binary to be on PATH or invokable via os.Executable")
	}

	res, err := install.Install(binPath)
	if err != nil {
		return err
	}

	fmt.Printf("agentrun: Claude hooks %s — %s\n", res.Claude.Action, res.Claude.Path)
	if res.Claude.Backup != "" {
		fmt.Printf("  (backup: %s)\n", res.Claude.Backup)
	}
	fmt.Printf("agentrun: Codex hooks %s — %s\n", res.Codex.Action, res.Codex.Path)
	if res.Codex.Backup != "" {
		fmt.Printf("  (backup: %s)\n", res.Codex.Backup)
	}

	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "agentrun: setup complete. Sessions will be recorded to ~/.agentrun/agentrun.db.")
	fmt.Fprintln(os.Stderr, "agentrun: NOTE — the first time you run Codex interactively, it will ask you to trust the")
	fmt.Fprintln(os.Stderr, "                 agentrun hook command. Type '/hooks' inside Codex and approve to enable recording.")
	fmt.Fprintln(os.Stderr, "                 Until trust is granted, Codex sessions record only the SessionStart payload.")
	return nil
}

// runUninstall removes agentrun's hook entries from the global config files,
// leaving any user-authored hooks intact.
func runUninstall(args []string) error {
	if len(args) > 0 {
		return ErrUsage
	}

	res, err := install.Uninstall()
	if err != nil {
		return err
	}

	fmt.Printf("agentrun: Claude hooks %s — %s", res.Claude.Action, res.Claude.Path)
	if res.Claude.Message != "" {
		fmt.Printf(" (%s)", res.Claude.Message)
	}
	fmt.Println()
	if res.Claude.Backup != "" {
		fmt.Printf("  (backup: %s)\n", res.Claude.Backup)
	}

	fmt.Printf("agentrun: Codex hooks %s — %s", res.Codex.Action, res.Codex.Path)
	if res.Codex.Message != "" {
		fmt.Printf(" (%s)", res.Codex.Message)
	}
	fmt.Println()
	if res.Codex.Backup != "" {
		fmt.Printf("  (backup: %s)\n", res.Codex.Backup)
	}
	return nil
}
