// Command ki-agent is a background agent that manages one Linux server's
// services for an external control plane.
package main

import (
	"os"

	"github.com/open-southeners/ki-agent/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(cli.ExitCode(err))
	}
}
