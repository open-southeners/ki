package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// accountUser mirrors the User object of the users.* methods.
type accountUser struct {
	Username     string  `json:"username"`
	Role         string  `json:"role"`
	Status       string  `json:"status"`
	TOTPEnrolled bool    `json:"totp_enrolled"`
	CreatedAt    string  `json:"created_at"`
	LastLoginAt  *string `json:"last_login_at"`
}

// setupLink mirrors the result of users.create, users.reset and panel.setup.
type setupLink struct {
	User              accountUser `json:"user"`
	SetupURL          string      `json:"setup_url"`
	FingerprintSHA256 string      `json:"fingerprint_sha256"`
	ExpiresAt         string      `json:"expires_at"`
}

// printSetupLink writes the one-time setup URL and, when present, the
// certificate fingerprint to check it against.
func printSetupLink(w io.Writer, username string, l *setupLink) {
	fmt.Fprintf(w, "Setup URL for %s:\n\n  %s\n\n", username, l.SetupURL)
	if l.FingerprintSHA256 != "" {
		fmt.Fprintf(w, "The panel uses a self-signed certificate. Check that its SHA-256 fingerprint is:\n\n  %s\n\n", l.FingerprintSHA256)
	}
	fmt.Fprintf(w, "This link is valid for 30 minutes and works once; share it only with %s.\n", username)
	fmt.Fprintln(w, "It is shown only once. Run the command again to issue a new one.")
}

func newUsersCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage panel users",
	}
	cmd.AddCommand(newUsersListCmd(f), newUsersCreateCmd(f), newUsersUpdateCmd(f),
		newUsersResetCmd(f), newUsersDeleteCmd(f))
	return cmd
}

func newUsersListCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "users.list", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var list []accountUser
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "No users.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "USERNAME\tROLE\tSTATUS\tTOTP\tCREATED\tLAST LOGIN")
			for _, u := range list {
				totp, last := "no", "-"
				if u.TOTPEnrolled {
					totp = "yes"
				}
				if u.LastLoginAt != nil {
					last = *u.LastLoginAt
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", u.Username, u.Role, u.Status, totp, u.CreatedAt, last)
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// callSetupLink calls a method returning a setup link and prints it.
func callSetupLink(cmd *cobra.Command, f *flags, method string, params map[string]any, username string, asJSON bool) error {
	var raw json.RawMessage
	if err := callOnce(cmd, f, method, params, &raw); err != nil {
		return err
	}
	if asJSON {
		return printJSON(cmd.OutOrStdout(), raw)
	}
	var l setupLink
	if err := json.Unmarshal(raw, &l); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	if l.User.Username != "" { // the agent's answer wins over the flag value
		username = l.User.Username
	}
	printSetupLink(cmd.OutOrStdout(), username, &l)
	return nil
}

func newUsersCreateCmd(f *flags) *cobra.Command {
	var (
		role   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "create <username> --role <viewer|operator|admin>",
		Short: "Create a user and print their one-time setup link",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return callSetupLink(cmd, f, "users.create",
				map[string]any{"username": args[0], "role": role}, args[0], asJSON)
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "role: viewer, operator or admin")
	_ = cmd.MarkFlagRequired("role")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newUsersUpdateCmd(f *flags) *cobra.Command {
	var (
		role            string
		disable, enable bool
		asJSON          bool
	)
	cmd := &cobra.Command{
		Use:   "update <username> [--role <role>] [--disable|--enable]",
		Short: "Change a user's role or disable or enable the account",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"username": args[0]}
			if cmd.Flags().Changed("role") {
				params["role"] = role
			}
			if disable {
				params["disabled"] = true
			}
			if enable {
				params["disabled"] = false
			}
			if len(params) == 1 {
				return errors.New("nothing to update: pass --role, --disable or --enable")
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "users.update", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var u accountUser
			if err := json.Unmarshal(raw, &u); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "User %s updated (role %s, %s)\n", u.Username, u.Role, u.Status)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "new role: viewer, operator or admin")
	cmd.Flags().BoolVar(&disable, "disable", false, "disable the account and end its sessions")
	cmd.Flags().BoolVar(&enable, "enable", false, "enable a disabled account")
	cmd.MarkFlagsMutuallyExclusive("disable", "enable")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newUsersResetCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "reset <username>",
		Short: "Clear a user's password and two-factor and print a new setup link",
		Long: `Clear a user's password and authenticator, end all of their sessions and
connections, and print a new one-time setup link.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return callSetupLink(cmd, f, "users.reset", map[string]any{"username": args[0]}, args[0], asJSON)
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newUsersDeleteCmd(f *flags) *cobra.Command {
	var yes, asJSON bool
	cmd := &cobra.Command{
		Use:   "delete <username>",
		Short: "Delete a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !yes {
				if err := confirmYes(cmd, fmt.Sprintf("This deletes user %s and ends their sessions.", args[0])); err != nil {
					return err
				}
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "users.delete", map[string]any{"username": args[0]}, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "User %s deleted\n", args[0])
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	addJSONFlag(cmd, &asJSON)
	return cmd
}
