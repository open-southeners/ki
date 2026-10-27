// Command ki is the Kiarga command-line tool. Its agent subcommands run and
// manage the background agent that controls one Linux server's services for
// an external control plane; invoked as ki-agent it behaves as `ki agent`.
package main

import (
	"os"

	"github.com/open-southeners/ki-agent/internal/cli"
)

func main() {
	if err := cli.Execute(os.Args[0]); err != nil {
		os.Exit(cli.ExitCode(err))
	}
}
