package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/AI4Everyonee/agentrun/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agentrun: %v\n", err)
		if errors.Is(err, cli.ErrUsage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
