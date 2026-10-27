package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// tunnelStatus mirrors tunnel.Status.
type tunnelStatus struct {
	Enabled     bool     `json:"enabled"`
	State       string   `json:"state"`
	Configured  bool     `json:"configured"`
	Ready       bool     `json:"ready"`
	Connections int      `json:"connections"`
	Image       string   `json:"image"`
	LastError   string   `json:"last_error"`
	Warnings    []string `json:"warnings"`
}

func newTunnelCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Manage the Cloudflare Tunnel that exposes this server without open ports",
	}
	cmd.AddCommand(newTunnelStatusCmd(f), newTunnelConfigureCmd(f), newTunnelRemoveCmd(f))
	return cmd
}

func printTunnelStatus(cmd *cobra.Command, st tunnelStatus) error {
	w := cmd.OutOrStdout()
	if !st.Enabled {
		fmt.Fprintln(w, "Tunnel: disabled (set tunnel.enabled: true to run the Cloudflare Tunnel connector)")
		if st.Configured {
			fmt.Fprintln(w, "A token is stored.")
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "State\t%s\n", st.State)
	if st.State == "unconfigured" {
		fmt.Fprintln(tw, "\tstore the tunnel token with: ki agent tunnel configure --token-stdin")
	}
	ready := "no"
	if st.Ready {
		ready = "yes"
	}
	fmt.Fprintf(tw, "Ready\t%s\n", ready)
	fmt.Fprintf(tw, "Connections\t%d\n", st.Connections)
	fmt.Fprintf(tw, "Image\t%s\n", st.Image)
	if st.LastError != "" {
		fmt.Fprintf(tw, "Last error\t%s\n", st.LastError)
	}
	for _, warn := range st.Warnings {
		fmt.Fprintf(tw, "Warning\t%s\n", warn)
	}
	return tw.Flush()
}

func newTunnelStatusCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the tunnel state and its connections to Cloudflare",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "tunnel.status", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var st tunnelStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			return printTunnelStatus(cmd, st)
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newTunnelConfigureCmd(f *flags) *cobra.Command {
	var (
		stdin  bool
		file   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "configure (--token-stdin | --token-file <file>)",
		Short: "Store the Cloudflare Tunnel token and start the connector",
		Long: `Stores the token of a remotely managed Cloudflare Tunnel (shown in the Zero Trust
dashboard when the tunnel is created) and starts the connector when
tunnel.enabled is true. The token is read from standard input or a file; there
is deliberately no token argument, which would leak through the process list
and shell history. It is stored in a root-only file and never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if stdin == (file != "") {
				return errors.New("pass exactly one of --token-stdin or --token-file")
			}
			var raw []byte
			var err error
			if stdin {
				raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 64<<10))
			} else {
				raw, err = os.ReadFile(file)
			}
			if err != nil {
				return fmt.Errorf("read token: %w", err)
			}
			token := strings.TrimSpace(string(raw))
			if token == "" {
				return errors.New("token is empty")
			}
			var res json.RawMessage
			if err := callOnce(cmd, f, "tunnel.configure", map[string]string{"token": token}, &res); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), res)
			}
			var st tunnelStatus
			if err := json.Unmarshal(res, &st); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Tunnel token stored.")
			return printTunnelStatus(cmd, st)
		},
	}
	cmd.Flags().BoolVar(&stdin, "token-stdin", false, "read the tunnel token from standard input")
	cmd.Flags().StringVar(&file, "token-file", "", "read the tunnel token from `file`")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newTunnelRemoveCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Delete the stored token and remove the tunnel connector",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "tunnel.remove", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Tunnel removed.")
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
