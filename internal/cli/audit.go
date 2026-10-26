package cli

import (
	"encoding/json"
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// auditEntry mirrors one element of audit.list.
type auditEntry struct {
	ID            int64  `json:"id"`
	Time          string `json:"time"`
	PrincipalKind string `json:"principal_kind"`
	PrincipalName string `json:"principal_name"`
	IP            string `json:"ip"`
	Action        string `json:"action"`
	Target        string `json:"target"`
	Result        string `json:"result"`
}

func (e *auditEntry) principal() string {
	switch {
	case e.PrincipalName == "":
		return e.PrincipalKind
	case e.PrincipalKind == "" || e.PrincipalKind == "user":
		return e.PrincipalName
	}
	return e.PrincipalKind + ":" + e.PrincipalName
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newAuditCmd(f *flags) *cobra.Command {
	var (
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "audit [--limit N]",
		Short: "Show the audit log, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := map[string]any{}
			if cmd.Flags().Changed("limit") {
				params["limit"] = limit
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "audit.list", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var list []auditEntry
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "No audit entries.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TIME\tPRINCIPAL\tIP\tACTION\tTARGET\tRESULT")
			for _, e := range list {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Time, e.principal(), dash(e.IP), e.Action, dash(e.Target), e.Result)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of entries to show (at most 500)")
	addJSONFlag(cmd, &asJSON)
	return cmd
}
