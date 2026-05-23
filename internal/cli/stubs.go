package cli

// Stubs for in-flight subcommands. Each is replaced by its real file as the
// corresponding feature lands. Keep these no-ops until then so root.go's
// dispatch compiles and `agentrun help` lists them.

import (
	"errors"
)

var errNotYetImplemented = errors.New("not yet implemented")

func runStats(args []string) error    { return errNotYetImplemented }
func runDiff(args []string) error     { return errNotYetImplemented }
func runSearch(args []string) error   { return errNotYetImplemented }
func runReplay(args []string) error   { return errNotYetImplemented }
func runCompare(args []string) error  { return errNotYetImplemented }
func runTag(args []string) error      { return errNotYetImplemented }
func runCollector(args []string) error { return errNotYetImplemented }
