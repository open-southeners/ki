// Package cli implements the ki command-line interface (the agent commands live under `ki agent`).
package cli

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki/internal/config"
	"github.com/open-southeners/ki/internal/version"
)

// flags holds the persistent flags shared by all subcommands.
type flags struct {
	configPath string
	logLevel   string
	socket     string
	noPanel    bool
}

// aliasName is the legacy binary name. Invoked under it, the binary behaves
// exactly like `ki agent`, so existing units, scripts and updaters keep working.
const aliasName = "ki-agent"

// newRootCmd builds the `ki` command tree.
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "ki",
		Short:         "Kiarga command-line tool; `ki agent` manages this server's agent",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newAgentCmd("agent"))
	return cmd
}

// newAgentCmd builds the agent command tree, named use: "agent" under `ki`,
// or the root itself when the binary runs as ki-agent.
func newAgentCmd(use string) *cobra.Command {
	var f flags

	cmd := &cobra.Command{
		Use:           use,
		Short:         "Run and manage the ki agent on this server",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&f.configPath, "config", config.DefaultPath, "path to the YAML config file")
	cmd.PersistentFlags().StringVar(&f.logLevel, "log-level", "", "log level: debug, info, warn, error (overrides config)")

	cmd.PersistentFlags().StringVar(&f.socket, "socket", "", "path to the agent's Unix socket for client commands (default: $KI_SOCKET_PATH, then the config file)")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newRunCmd(&f))
	cmd.AddCommand(newProjectsCmd(&f))
	cmd.AddCommand(newStatusCmd(&f))
	cmd.AddCommand(newEdgeCmd(&f))
	cmd.AddCommand(newLogsCmd(&f))
	cmd.AddCommand(newDeployCmd(&f))
	cmd.AddCommand(newDeploysCmd(&f))
	cmd.AddCommand(newEnrollCmd(&f))
	cmd.AddCommand(newUnenrollCmd(&f))
	cmd.AddCommand(newUsersCmd(&f))
	cmd.AddCommand(newTokensCmd(&f))
	cmd.AddCommand(newPanelCmd(&f))
	cmd.AddCommand(newAuditCmd(&f))
	cmd.AddCommand(newStatsCmd(&f))
	cmd.AddCommand(newContainersCmd(&f))
	cmd.AddCommand(newServicesCmd(&f))
	cmd.AddCommand(newAWSCmd(&f))
	cmd.AddCommand(newTunnelCmd(&f))
	cmd.AddCommand(newUpdateCmd(&f))

	return cmd
}

// rootFor returns the command tree for a binary invoked as argv0: the agent
// tree itself under the ki-agent name, the `ki` tree otherwise.
func rootFor(argv0 string) *cobra.Command {
	if filepath.Base(argv0) == aliasName {
		return newAgentCmd(aliasName)
	}
	return newRootCmd()
}

// Execute runs the command tree selected by argv0 (os.Args[0]), printing any
// error before returning it to the caller.
func Execute(argv0 string) error {
	cmd := rootFor(argv0)
	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln(err)
		return err
	}
	return nil
}
