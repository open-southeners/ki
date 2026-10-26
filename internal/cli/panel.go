package cli

import (
	"github.com/spf13/cobra"
)

func newPanelCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "panel",
		Short: "Manage the web panel",
	}
	cmd.AddCommand(newPanelSetupCmd(f))
	return cmd
}

func newPanelSetupCmd(f *flags) *cobra.Command {
	var (
		username string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "setup [--username admin]",
		Short: "Print the link to create the first panel admin",
		Long: `Print the one-time link that creates the first admin of the web panel.

It only works while no admin exists; afterwards use "ki-agent users reset
<username>" to issue a new link.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := map[string]any{}
			if cmd.Flags().Changed("username") {
				params["username"] = username
			}
			return callSetupLink(cmd, f, "panel.setup", params, username, asJSON)
		},
	}
	cmd.Flags().StringVar(&username, "username", "admin", "username of the admin to create")
	addJSONFlag(cmd, &asJSON)
	return cmd
}
