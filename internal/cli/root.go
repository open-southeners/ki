// Package cli implements the ki-agent command-line interface.
package cli

import (
	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/version"
)

// flags holds the persistent flags shared by all subcommands.
type flags struct {
	configPath string
	logLevel   string
}

func newRootCmd() *cobra.Command {
	var f flags

	cmd := &cobra.Command{
		Use:           "ki-agent",
		Short:         "Background agent managing a Linux server's services",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&f.configPath, "config", config.DefaultPath, "path to the YAML config file")
	cmd.PersistentFlags().StringVar(&f.logLevel, "log-level", "", "log level: debug, info, warn, error (overrides config)")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newRunCmd(&f))

	return cmd
}

// Execute runs the ki-agent root command, printing any error before
// returning it to the caller.
func Execute() error {
	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln(err)
		return err
	}
	return nil
}
