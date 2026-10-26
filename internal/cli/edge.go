package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/edge/certs"
)

// edgeStatus mirrors EdgeStatus.
type edgeStatus struct {
	Enabled         bool     `json:"enabled"`
	State           string   `json:"state"`
	LastError       string   `json:"last_error"`
	Image           string   `json:"image"`
	HTTPPort        int      `json:"http_port"`
	HTTPSPort       int      `json:"https_port"`
	Mode            string   `json:"mode"`
	Listen          string   `json:"listen"`
	ServerHostnames []string `json:"server_hostnames"`
	SelfSigned      struct {
		Fingerprint string `json:"fingerprint_sha256"`
		NotAfter    string `json:"not_after"`
	} `json:"self_signed"`
	Certificates []struct {
		ID       string   `json:"id"`
		Hostname string   `json:"hostname"`
		Names    []string `json:"names"`
		Source   string   `json:"source"`
		Server   bool     `json:"server"`
		NotAfter string   `json:"not_after"`
	} `json:"certificates"`
	Routes int `json:"routes"`
}

func newEdgeCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edge",
		Short: "Inspect the edge proxy that publishes exposed services",
	}
	cmd.AddCommand(newEdgeStatusCmd(f), newEdgeCertificateCmd(f), newEdgeProxyConfigCmd(f))
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
			if st.Mode == "behind" {
				fmt.Fprintf(tw, "Mode\tbehind (plain HTTP on %s, TLS on the front proxy)\n", st.Listen)
			} else {
				fmt.Fprintf(tw, "Mode\tfront\n")
				fmt.Fprintf(tw, "Ports\thttp %d, https %d\n", st.HTTPPort, st.HTTPSPort)
			}
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
			fmt.Fprintln(tw, "  ID\tNAMES\tSOURCE\tSERVER\tEXPIRES")
			for _, c := range st.Certificates {
				exp := c.NotAfter
				if exp == "" {
					exp = "-"
				}
				names := strings.Join(c.Names, ", ")
				if names == "" {
					names = c.Hostname
				}
				id := c.ID
				if id == "" {
					id = c.Hostname
				}
				server := "no"
				if c.Server {
					server = "yes"
				}
				fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", id, names, c.Source, server, exp)
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newEdgeCertificateCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certificate",
		Short: "Manage bring-your-own and installed edge certificates",
	}
	cmd.AddCommand(newEdgeCertImportCmd(f), newEdgeCertRemoveCmd(f))
	return cmd
}

func newEdgeCertImportCmd(f *flags) *cobra.Command {
	var certFile, keyFile string
	var server, asJSON bool
	cmd := &cobra.Command{
		Use:   "import --cert <file> --key <file> [--server]",
		Short: "Import a certificate and private key to serve project domains",
		Long: "Imports a PEM certificate chain and its private key (RSA, ECDSA or Ed25519).\n" +
			"The certificate's DNS names (wildcards such as *.example.com allowed) are served\n" +
			"from it instead of requesting ACME certificates. With --server its non-wildcard\n" +
			"names also become server hostnames (panel and webhooks). The files are read as\n" +
			"the invoking user and sent over the local socket only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if certFile == "" || keyFile == "" {
				return fmt.Errorf("--cert and --key are required")
			}
			certPEM, err := os.ReadFile(certFile)
			if err != nil {
				return err
			}
			keyPEM, err := os.ReadFile(keyFile)
			if err != nil {
				return err
			}
			var raw json.RawMessage
			params := map[string]any{"certificate": string(certPEM), "key": string(keyPEM), "server": server}
			if err := callOnce(cmd, f, "edge.certificate.import", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var st edgeStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			// The id derives from the names, so the entry can be found
			// without trusting the order of the status.
			if names, err := certs.ImportNames(certPEM, keyPEM, time.Now()); err == nil {
				id := certs.ImportedID(names)
				for _, c := range st.Certificates {
					if c.ID == id {
						fmt.Fprintf(cmd.OutOrStdout(), "Imported %s (%s), expires %s\n", c.ID, strings.Join(c.Names, ", "), c.NotAfter)
						return nil
					}
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Certificate imported.")
			return nil
		},
	}
	cmd.Flags().StringVar(&certFile, "cert", "", "PEM certificate chain file")
	cmd.Flags().StringVar(&keyFile, "key", "", "PEM private key file")
	cmd.Flags().BoolVar(&server, "server", false, "also use the non-wildcard names as server hostnames")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newEdgeCertRemoveCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "remove <id|hostname>",
		Short: "Remove an imported certificate (by id) or an installed one (by hostname)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]string{"hostname": args[0]}
			if certs.ValidID(args[0]) {
				params = map[string]string{"id": args[0]}
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "edge.certificate.remove", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s.\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
