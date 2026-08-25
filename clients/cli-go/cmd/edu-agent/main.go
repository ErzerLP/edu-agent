package main

import (
	"context"
	"fmt"
	"os"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/command"
)

var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	app, err := command.NewDefault(os.Stdin, os.Stdout, os.Stderr, command.BuildInfo{Version: version, Commit: commit})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error[local_setup_failed]: local configuration paths are unavailable; next: inspect the user configuration directory")
		os.Exit(command.ExitInternal)
	}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
