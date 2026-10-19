package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

func newStatusCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the agent, its container engine and project count",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			defer c.Close()

			var info json.RawMessage
			if err := c.Call(cmd.Context(), "agent.info", nil, &info); err != nil {
				return explainError(err)
			}
			// Engine and project count are best effort.
			var engine json.RawMessage
			engineErr := c.Call(cmd.Context(), "runtime.info", nil, &engine)
			var projects json.RawMessage
			projErr := c.Call(cmd.Context(), "projects.list", nil, &projects)

			if asJSON {
				out := map[string]any{"agent": info, "runtime": nil, "projects": nil}
				if engineErr == nil {
					out["runtime"] = engine
				}
				if projErr == nil {
					out["projects"] = projects
				}
				raw, err := json.Marshal(out)
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), raw)
			}

			var ai struct {
				Version      string `json:"version"`
				Commit       string `json:"commit"`
				Hostname     string `json:"hostname"`
				OS           string `json:"os"`
				Arch         string `json:"arch"`
				UptimeMS     int64  `json:"uptime_ms"`
				ControlPlane struct {
					State          string `json:"state"`
					URL            string `json:"url"`
					ConnectedSince string `json:"connected_since"`
					LastError      string `json:"last_error"`
				} `json:"control_plane"`
			}
			if err := json.Unmarshal(info, &ai); err != nil {
				return fmt.Errorf("decode agent.info: %w", err)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			agent := "ki-agent " + ai.Version
			if ai.Commit != "" {
				agent += " (" + ai.Commit + ")"
			}
			fmt.Fprintf(tw, "Agent\t%s\n", agent)
			fmt.Fprintf(tw, "Host\t%s (%s/%s)\n", ai.Hostname, ai.OS, ai.Arch)
			fmt.Fprintf(tw, "Uptime\t%s\n", (time.Duration(ai.UptimeMS) * time.Millisecond).Round(time.Second))
			cp := ai.ControlPlane.State
			if cp == "" {
				cp = "unknown"
			}
			if ai.ControlPlane.URL != "" {
				cp += " " + ai.ControlPlane.URL
			}
			if ai.ControlPlane.State == "connected" && ai.ControlPlane.ConnectedSince != "" {
				cp += " since " + ai.ControlPlane.ConnectedSince
			}
			if ai.ControlPlane.LastError != "" && ai.ControlPlane.State != "connected" {
				cp += " (last error: " + ai.ControlPlane.LastError + ")"
			}
			fmt.Fprintf(tw, "Control plane\t%s\n", cp)

			if engineErr != nil {
				fmt.Fprintf(tw, "Engine\tunavailable: %s\n", explainError(engineErr))
			} else {
				var ei struct {
					Name       string `json:"name"`
					Version    string `json:"version"`
					APIVersion string `json:"api_version"`
				}
				_ = json.Unmarshal(engine, &ei)
				fmt.Fprintf(tw, "Engine\t%s %s (API %s)\n", ei.Name, ei.Version, ei.APIVersion)
			}

			switch {
			case projErr == nil:
				var list []json.RawMessage
				_ = json.Unmarshal(projects, &list)
				fmt.Fprintf(tw, "Projects\t%d\n", len(list))
			case isMethodNotFound(projErr):
				fmt.Fprintf(tw, "Projects\tnot supported by this agent\n")
			default:
				fmt.Fprintf(tw, "Projects\tunavailable: %s\n", explainError(projErr))
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func isMethodNotFound(err error) bool {
	var re *rpc.Error
	return errors.As(err, &re) && re.Code == rpc.CodeMethodNotFound
}
