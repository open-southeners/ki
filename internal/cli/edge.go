package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// edgeStatus mirrors EdgeStatus.
type edgeStatus struct {
	Enabled         bool     `json:"enabled"`
	State           string   `json:"state"`
	LastError       string   `json:"last_error"`
	Image           string   `json:"image"`
	HTTPPort        int      `json:"http_port"`
	HTTPSPort       int      `json:"https_port"`
	ServerHostnames []string `json:"server_hostnames"`
	SelfSigned      struct {
		Fingerprint string `json:"fingerprint_sha256"`
		NotAfter    string `json:"not_after"`
	} `json:"self_signed"`
	Certificates []struct {
		Hostname string `json:"hostname"`
		Source   string `json:"source"`
		NotAfter string `json:"not_after"`
	} `json:"certificates"`
	Routes int `json:"routes"`
}

func newEdgeCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edge",
		Short: "Inspect the edge proxy that publishes exposed services",
	}
	cmd.AddCommand(newEdgeStatusCmd(f))
	return cmd
}

func newEdgeStatusCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the edge proxy state, certificates and routes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "edge.status", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var st edgeStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if !st.Enabled {
				fmt.Fprintln(w, "Edge: disabled (set edge.enabled: true to publish services)")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintf(tw, "State\t%s\n", st.State)
			fmt.Fprintf(tw, "Ports\thttp %d, https %d\n", st.HTTPPort, st.HTTPSPort)
			fmt.Fprintf(tw, "Image\t%s\n", st.Image)
			hosts := "-"
			if len(st.ServerHostnames) > 0 {
				hosts = strings.Join(st.ServerHostnames, ", ")
			}
			fmt.Fprintf(tw, "Server hostnames\t%s\n", hosts)
			fmt.Fprintf(tw, "Self-signed\tSHA-256 %s, expires %s\n", st.SelfSigned.Fingerprint, st.SelfSigned.NotAfter)
			fmt.Fprintf(tw, "Routes\t%d\n", st.Routes)
			if st.LastError != "" {
				fmt.Fprintf(tw, "Last error\t%s\n", st.LastError)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			if len(st.Certificates) == 0 {
				fmt.Fprintln(w, "\nNo certificates.")
				return nil
			}
			fmt.Fprintln(w, "\nCertificates:")
			tw = tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "  HOSTNAME\tSOURCE\tEXPIRES")
			for _, c := range st.Certificates {
				exp := c.NotAfter
				if exp == "" {
					exp = "-"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\n", c.Hostname, c.Source, exp)
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
