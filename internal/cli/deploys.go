package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/client"
)

// Method and notification names of the deploy log stream.
const (
	methodDeploysSubscribe = "deploys.subscribe"
	notifyDeploysUpdate    = "deploys.update"
)

// gapPollInterval is how often a stream that lost updates re-reads the
// deploy, because the final status update may be among the lost ones.
const gapPollInterval = 3 * time.Second

// logsIdleGrace is how long "deploys logs" without --follow waits for further
// replayed lines of an unfinished deploy before it stops.
const logsIdleGrace = 500 * time.Millisecond

// deploy mirrors the Deploy object of the deploys.* methods.
type deploy struct {
	ID         int64        `json:"id"`
	Project    string       `json:"project"`
	Ref        string       `json:"ref"`
	Commit     string       `json:"commit"`
	Source     string       `json:"source"`
	Status     string       `json:"status"`
	Stage      string       `json:"stage"`
	Error      *deployError `json:"error"`
	CreatedAt  string       `json:"created_at"`
	StartedAt  *string      `json:"started_at"`
	FinishedAt *string      `json:"finished_at"`
}

// violation is one policy breach recorded on a failed deploy.
type violation struct {
	Service string `json:"service"`
	Rule    string `json:"rule"`
	Detail  string `json:"detail"`
}

// deployError is the error recorded on a failed deploy.
type deployError struct {
	Reason     string      `json:"reason"`
	Detail     string      `json:"detail"`
	Violations []violation `json:"violations"`
}

// finished reports whether the deploy reached a terminal status.
func (d *deploy) finished() bool {
	switch d.Status {
	case "queued", "running":
		return false
	}
	return true
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func newDeployCmd(f *flags) *cobra.Command {
	var (
		slug, ref      string
		detach, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "deploy --project <slug> [--ref <ref>]",
		Short: "Deploy a project and follow the deploy log",
		Long: `Trigger a deploy of a project and follow its log until it finishes.

Without --ref the project's trigger decides: the head of its branch, or the
newest tag matching its tag pattern. --ref accepts refs/heads/<branch>,
refs/tags/<tag>, a branch or tag name, or a full commit hash that the trigger
allows. The command exits non-zero when the deploy fails or is interrupted.
With --detach it prints the queued deploy and returns at once.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			params := map[string]any{"project": slug}
			if ref != "" {
				params["ref"] = ref
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			defer c.Close()

			var raw json.RawMessage
			if err := c.Call(ctx, "deploys.trigger", params, &raw); err != nil {
				return explainError(err)
			}
			var d deploy
			if err := json.Unmarshal(raw, &d); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if detach {
				if asJSON {
					return printJSON(w, raw)
				}
				fmt.Fprintf(w, "Deploy %d %s for project %s (ref %s)\n", d.ID, d.Status, d.Project, d.Ref)
				return nil
			}
			if !asJSON {
				fmt.Fprintf(w, "Deploy %d %s for project %s (ref %s)\n", d.ID, d.Status, d.Project, d.Ref)
			}
			return followDeploy(ctx, cmd, c, d.ID, true, asJSON)
		},
	}
	cmd.Flags().StringVar(&slug, "project", "", "project `slug` to deploy")
	cmd.Flags().StringVar(&ref, "ref", "", "branch, tag or commit to deploy (default: the project's trigger)")
	cmd.Flags().BoolVar(&detach, "detach", false, "do not wait for the deploy to finish")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the deploy (--detach) or each raw log update as JSON")
	_ = cmd.MarkFlagRequired("project")
	return cmd
}

func newDeploysCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploys",
		Short: "Inspect a project's deploys",
	}
	cmd.AddCommand(newDeploysListCmd(f), newDeploysGetCmd(f), newDeploysLogsCmd(f))
	return cmd
}

func newDeploysListCmd(f *flags) *cobra.Command {
	var (
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "list <slug>",
		Short: "List a project's deploys, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"project": args[0]}
			if cmd.Flags().Changed("limit") {
				params["limit"] = limit
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "deploys.list", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var list []deploy
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "No deploys.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATUS\tSTAGE\tREF\tCOMMIT\tSOURCE\tCREATED")
			for _, d := range list {
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
					d.ID, d.Status, d.Stage, d.Ref, shortCommit(d.Commit), d.Source, d.CreatedAt)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of deploys to list (at most 100)")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// parseDeployID parses the <id> argument of the deploys subcommands.
func parseDeployID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid deploy id %q: want a positive number", s)
	}
	return id, nil
}

func newDeploysGetCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Show a deploy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseDeployID(args[0])
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "deploys.get", map[string]any{"id": id}, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var d deploy
			if err := json.Unmarshal(raw, &d); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			printDeploy(cmd.OutOrStdout(), &d)
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newDeploysLogsCmd(f *flags) *cobra.Command {
	var follow, asJSON bool
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Show a deploy's log",
		Long: `Show a deploy's stored log. A finished deploy prints its whole log, and the
command exits non-zero if the deploy did not succeed. For a deploy that is still
running, --follow keeps streaming until it finishes; without it only the lines
logged so far are printed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseDeployID(args[0])
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			defer c.Close()
			return followDeploy(ctx, cmd, c, id, follow, asJSON)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "keep streaming until the deploy finishes")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print each raw log update as a JSON line")
	return cmd
}

// followDeploy subscribes to a deploy and prints its log. With follow it waits
// for the final status; without it, an unfinished deploy stops once the stored
// lines have been replayed. It returns an error when the deploy ended
// unsuccessfully.
func followDeploy(ctx context.Context, cmd *cobra.Command, c *client.Client, id int64, follow, asJSON bool) error {
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	var (
		final    *deploy
		last     int64
		gapped   = make(chan struct{}, 1)
		activity = make(chan struct{}, 1)
	)
	sub, err := c.Subscribe(ctx, methodDeploysSubscribe, map[string]any{"id": id}, notifyDeploysUpdate,
		func(seq int64, data json.RawMessage) bool {
			prev := last
			last = seq
			defer func() {
				select {
				case activity <- struct{}{}:
				default:
				}
			}()
			if len(data) == 0 {
				select {
				case gapped <- struct{}{}:
				default:
				}
				fmt.Fprintf(stderr, "-- %d log lines were dropped --\n", seq-prev)
				return true
			}
			if asJSON {
				fmt.Fprintln(stdout, string(data))
			}
			var u struct {
				Type   string  `json:"type"`
				Stream string  `json:"stream"`
				Text   string  `json:"text"`
				Deploy *deploy `json:"deploy"`
			}
			if err := json.Unmarshal(data, &u); err != nil {
				return true
			}
			switch u.Type {
			case "status":
				final = u.Deploy
				return false
			case "log":
				if !asJSON {
					printDeployLine(stdout, stderr, u.Stream, u.Text)
				}
			}
			return true
		})
	if err != nil {
		return explainError(err)
	}

	closeSub := func() {
		uctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = sub.Close(uctx)
	}

	// idle ends a non-following run once the replay goes quiet.
	var idle *time.Timer
	var idleC <-chan time.Time
	if !follow {
		idle = time.NewTimer(logsIdleGrace)
		defer idle.Stop()
		idleC = idle.C
	}
	var poll *time.Ticker
	var pollC <-chan time.Time
	defer func() {
		if poll != nil {
			poll.Stop()
		}
	}()

	for {
		select {
		case <-sub.Done():
			if final == nil {
				return errors.New("connection to the agent was lost")
			}
			return deployOutcome(final)
		case <-ctx.Done():
			closeSub()
			return nil
		case <-c.Done():
			return errors.New("connection to the agent was lost")
		case <-activity:
			if idle != nil {
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(logsIdleGrace)
			}
		case <-gapped:
			if follow && poll == nil {
				poll = time.NewTicker(gapPollInterval)
				pollC = poll.C
			}
		case <-idleC:
			closeSub()
			return nil
		case <-pollC:
			// Updates were lost; the final status may have been among them.
			var d deploy
			if err := c.Call(ctx, "deploys.get", map[string]any{"id": id}, &d); err == nil && d.finished() {
				closeSub()
				return deployOutcome(&d)
			}
		}
	}
}

// printDeployLine writes one log line: agent lines are marked, stderr lines go
// to stderr.
func printDeployLine(stdout, stderr io.Writer, stream, text string) {
	text = strings.TrimRight(text, "\r\n")
	switch stream {
	case "agent":
		fmt.Fprintf(stdout, "==> %s\n", text)
	case "stderr":
		fmt.Fprintln(stderr, text)
	default:
		fmt.Fprintln(stdout, text)
	}
}

// deployOutcome turns a final deploy into nil (succeeded) or an error that
// carries the failure reason, detail and policy violations.
func deployOutcome(d *deploy) error {
	switch d.Status {
	case "succeeded":
		return nil
	case "failed":
		return deployFailure(d)
	}
	return fmt.Errorf("deploy %d %s", d.ID, d.Status)
}

// deployFailure formats a failed deploy as a multi-line error.
func deployFailure(d *deploy) error {
	msg := fmt.Sprintf("deploy %d failed", d.ID)
	if d.Stage != "" {
		msg += " at stage " + d.Stage
	}
	if e := d.Error; e != nil {
		if e.Reason != "" {
			msg += ": " + e.Reason
		}
		if e.Detail != "" {
			msg += ": " + e.Detail
		}
		if len(e.Violations) > 0 {
			var b strings.Builder
			writeViolations(&b, "", e.Violations)
			msg += "\n" + strings.TrimRight(b.String(), "\n")
		}
	}
	return errors.New(msg)
}

// writeViolations writes the policy violations as an aligned table, every line
// starting with indent.
func writeViolations(w io.Writer, indent string, vs []violation) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%sSERVICE\tRULE\tDETAIL\n", indent)
	for _, v := range vs {
		svc := v.Service
		if svc == "" {
			svc = "-"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\n", indent, svc, v.Rule, v.Detail)
	}
	_ = tw.Flush()
}

// printDeploy writes the aligned key/value view of a deploy.
func printDeploy(w io.Writer, d *deploy) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("ID", strconv.FormatInt(d.ID, 10))
	row("Project", d.Project)
	row("Ref", d.Ref)
	row("Commit", shortCommit(d.Commit))
	row("Source", d.Source)
	row("Status", d.Status)
	row("Stage", d.Stage)
	if d.Error != nil {
		e := d.Error.Reason
		if d.Error.Detail != "" {
			e += ": " + d.Error.Detail
		}
		row("Error", e)
	}
	row("Created", d.CreatedAt)
	if d.StartedAt != nil {
		row("Started", *d.StartedAt)
	}
	if d.FinishedAt != nil {
		row("Finished", *d.FinishedAt)
	}
	_ = tw.Flush()
	if d.Error != nil && len(d.Error.Violations) > 0 {
		fmt.Fprintln(w, "\nPolicy violations:")
		writeViolations(w, "  ", d.Error.Violations)
	}
}
