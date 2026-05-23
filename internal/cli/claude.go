package cli

// runClaude wraps the `claude` binary with the given passthrough args.
// It delegates to the shared runAgent function.
func runClaude(args []string) error {
	return runAgent("claude", args)
}
