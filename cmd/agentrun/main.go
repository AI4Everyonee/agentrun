package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/AI4Everyonee/agentrun/internal/cli"
)

// Build-time variables populated by goreleaser via -ldflags.
// Defaults are useful for `go install`-style local builds.
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Printf("agentrun %s\ncommit: %s\nbuilt:  %s\n", version, commit, date)
			return
		}
	}

	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: %v\n", err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
