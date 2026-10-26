package cli

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// apiToken mirrors one element of tokens.list.
type apiToken struct {
	Name       string  `json:"name"`
	Role       string  `json:"role"`
	CreatedAt  string  `json:"created_at"`
	CreatedBy  string  `json:"created_by"`
	ExpiresAt  *string `json:"expires_at"`
	LastUsedAt *string `json:"last_used_at"`
}

// durationParam converts s to a Go duration string; a "d" suffix counts days.
func durationParam(s string) (string, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days <= 0 {
			return "", fmt.Errorf("invalid --expires-in %q: want e.g. 90d, 720h", s)
		}
		return strconv.Itoa(days*24) + "h", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return "", fmt.Errorf("invalid --expires-in %q: want e.g. 90d, 720h", s)
	}
	return s, nil
}

func newTokensCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage API tokens",
	}
	cmd.AddCommand(newTokensCreateCmd(f), newTokensListCmd(f), newTokensRevokeCmd(f))
	return cmd
}

func newTokensCreateCmd(f *flags) *cobra.Command {
	var (
		role, expires string
		asJSON        bool
	)
	cmd := &cobra.Command{
		Use:   "create <name> --role <viewer|operator|admin> [--expires-in 90d]",
		Short: "Create an API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"name": args[0], "role": role}
			if cmd.Flags().Changed("expires-in") {
				d, err := durationParam(expires)
				if err != nil {
					return err
				}
				params["expires_in"] = d
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "tokens.create", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var res struct {
				Token     string  `json:"token"`
				Name      string  `json:"name"`
				Role      string  `json:"role"`
				ExpiresAt *string `json:"expires_at"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Token %s created (role %s", res.Name, res.Role)
			if res.ExpiresAt != nil {
				fmt.Fprintf(w, ", expires %s", *res.ExpiresAt)
			}
			fmt.Fprintf(w, ")\n\n  %s\n\n", res.Token)
			fmt.Fprintln(w, "WARNING: this token is shown only once and cannot be retrieved later.")
			fmt.Fprintln(w, "Store it somewhere safe; send it as \"Authorization: Bearer <token>\".")
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "role: viewer, operator or admin")
	cmd.Flags().StringVar(&expires, "expires-in", "", "lifetime of the token, e.g. 90d or 720h (default: never expires; at most 1 year)")
	_ = cmd.MarkFlagRequired("role")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newTokensListCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API tokens",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "tokens.list", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var list []apiToken
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "No API tokens.")
				return nil
			}
			orDash := func(s *string) string {
				if s == nil {
					return "-"
				}
				return *s
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tROLE\tCREATED\tCREATED BY\tEXPIRES\tLAST USED")
			for _, t := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
					t.Name, t.Role, t.CreatedAt, t.CreatedBy, orDash(t.ExpiresAt), orDash(t.LastUsedAt))
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newTokensRevokeCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "revoke <name>",
		Short: "Revoke an API token and close its connections",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "tokens.revoke", map[string]any{"name": args[0]}, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Token %s revoked\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
