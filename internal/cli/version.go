package cli

import (
	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/version"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the ki version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cmd.Printf("ki %s (commit %s)\n", version.Version, version.Commit)
		},
	}
}
