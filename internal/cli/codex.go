package cli

// runCodex wraps the `codex` binary with the given passthrough args.
// It delegates to the shared runAgent function.
func runCodex(args []string) error {
	return runAgent("codex", args)
}
