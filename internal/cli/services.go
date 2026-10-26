package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// projectService mirrors the Service object of projects.services.
type projectService struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    string `json:"state"`
	Exposure *struct {
		Port    int      `json:"port"`
		Domains []string `json:"domains"`
	} `json:"exposure"`
	Containers []struct {
		State  string `json:"state"`
		Health string `json:"health"`
	} `json:"containers"`
}

// health summarises the containers' health checks: unhealthy beats starting
// beats healthy; "-" when none of them defines a check.
func (s *projectService) health() string {
	var healthy, starting int
	for _, c := range s.Containers {
		switch c.Health {
		case "unhealthy":
			return "unhealthy"
		case "starting":
			starting++
		case "healthy":
			healthy++
		}
	}
	switch {
	case starting > 0:
		return "starting"
	case healthy > 0:
		return "healthy"
	}
	return "-"
}

func printServices(w io.Writer, list []projectService) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSTATE\tCONTAINERS\tHEALTH\tPORT\tDOMAINS")
	for _, s := range list {
		running := 0
		for _, c := range s.Containers {
			if c.State == "running" {
				running++
			}
		}
		port, domains := "-", "-"
		if s.Exposure != nil {
			port = fmt.Sprint(s.Exposure.Port)
			if len(s.Exposure.Domains) > 0 {
				domains = strings.Join(s.Exposure.Domains, ",")
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%d/%d\t%s\t%s\t%s\n", s.Name, s.State, running, len(s.Containers), s.health(), port, domains)
	}
	return tw.Flush()
}

func newServicesCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "services",
		Short: "List and control the services of a project",
	}
	cmd.AddCommand(newServicesListCmd(f))
	for _, verb := range []string{"start", "stop", "restart"} {
		cmd.AddCommand(newServicesActionCmd(f, verb))
	}
	return cmd
}

func newServicesListCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list <slug>",
		Short: "List a project's services with their state and health",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.services", map[string]any{"slug": args[0]}, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var res struct {
				Services []projectService `json:"services"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			if len(res.Services) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No services.")
				return nil
			}
			return printServices(cmd.OutOrStdout(), res.Services)
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newServicesActionCmd(f *flags, verb string) *cobra.Command {
	var asJSON bool
	past := map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"}[verb]
	cmd := &cobra.Command{
		Use:   verb + " <slug> <service>",
		Short: strings.ToUpper(verb[:1]) + verb[1:] + " every container of a project service",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw json.RawMessage
			params := map[string]any{"slug": args[0], "service": args[1]}
			if err := callOnce(cmd, f, "projects.services."+verb, params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var s projectService
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Service %s of project %s %s.\n\n", args[1], args[0], past)
			return printServices(cmd.OutOrStdout(), []projectService{s})
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}
