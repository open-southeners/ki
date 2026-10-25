package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// exposure mirrors the Exposure object of the projects.* methods.
type exposure struct {
	Service string   `json:"service"`
	Port    int      `json:"port"`
	Domains []string `json:"domains"`
	Path    string   `json:"path"`
	Source  string   `json:"source"`
}

// printExposure writes the effective exposure table and any warnings.
func printExposure(w io.Writer, p *project) {
	if len(p.Exposure) > 0 {
		fmt.Fprintln(w, "\nExposure:")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  SERVICE\tPORT\tDOMAINS\tPATH\tSOURCE")
		for _, e := range p.Exposure {
			path := e.Path
			if path == "" {
				path = "-"
			}
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%s\t%s\n", e.Service, e.Port, strings.Join(e.Domains, ","), path, e.Source)
		}
		_ = tw.Flush()
	}
	if len(p.ExposureWarnings) > 0 {
		fmt.Fprintln(w, "\nExposure warnings:")
		for _, msg := range p.ExposureWarnings {
			fmt.Fprintf(w, "  - %s\n", msg)
		}
	}
}

func newProjectsExposeCmd(f *flags) *cobra.Command {
	var (
		port    int
		domains []string
		path    string
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "expose <slug> <service> --port N --domain d [--domain d2] [--path /x]",
		Short: "Publish a project service on one or more domains through the edge",
		Long: `Publish a project service on one or more domains through the edge proxy.

The exposure overrides any ki.expose.* labels on the same service and survives
redeploys. Point the domains' DNS at this host (or its CDN).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"slug": args[0], "service": args[1], "port": port, "domains": domains}
			if path != "" {
				params["path"] = path
			}
			return runExposure(cmd, f, "projects.expose", params, asJSON, "exposed")
		},
	}
	cmd.Flags().IntVar(&port, "port", 0, "container `port` the service listens on")
	cmd.Flags().StringArrayVar(&domains, "domain", nil, "domain to serve (repeatable)")
	cmd.Flags().StringVar(&path, "path", "", "only route requests under this path `prefix`")
	_ = cmd.MarkFlagRequired("port")
	_ = cmd.MarkFlagRequired("domain")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newProjectsUnexposeCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "unexpose <slug> <service>",
		Short: "Remove a service exposure set with \"projects expose\"",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"slug": args[0], "service": args[1]}
			return runExposure(cmd, f, "projects.unexpose", params, asJSON, "unexposed")
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func runExposure(cmd *cobra.Command, f *flags, method string, params map[string]any, asJSON bool, verb string) error {
	var raw json.RawMessage
	if err := callOnce(cmd, f, method, params, &raw); err != nil {
		return err
	}
	if asJSON {
		return printJSON(cmd.OutOrStdout(), raw)
	}
	var p project
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Service %s of project %s %s.\n", params["service"], p.Slug, verb)
	if len(p.Exposure) == 0 && len(p.ExposureWarnings) == 0 {
		fmt.Fprintln(w, "\nNo services are exposed.")
	}
	printExposure(w, &p)
	return nil
}
