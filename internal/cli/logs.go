package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// Method and notification names of the container log stream.
const (
	methodLogsSubscribe = "containers.logs.subscribe"
	notifyLogsUpdate    = "containers.logs.update"
)

func newLogsCmd(f *flags) *cobra.Command {
	var (
		tail       int
		since      string
		timestamps bool
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "logs <container>",
		Short: "Follow a managed container's logs",
		Long: `Follow a managed container's logs until the container stops or Ctrl-C.

--since accepts an RFC 3339 timestamp or a duration such as 10m.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"id": args[0]}
			if cmd.Flags().Changed("tail") {
				params["tail"] = tail
			}
			if since != "" {
				ts, err := parseSince(since, time.Now())
				if err != nil {
					return err
				}
				params["since"] = ts
			}
			if timestamps {
				params["timestamps"] = true
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			defer c.Close()

			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			var last int64
			var streamErr error
			sub, err := c.Subscribe(ctx, methodLogsSubscribe, params, notifyLogsUpdate,
				func(seq int64, data json.RawMessage) bool {
					prev := last
					last = seq
					if len(data) == 0 {
						fmt.Fprintf(stderr, "-- %d log lines were dropped --\n", seq-prev)
						return true
					}
					if asJSON {
						fmt.Fprintln(stdout, string(data))
					}
					var u struct {
						Stream string `json:"stream"`
						Time   string `json:"time"`
						Text   string `json:"text"`
						EOF    bool   `json:"eof"`
						Error  string `json:"error"`
					}
					if err := json.Unmarshal(data, &u); err != nil {
						return true
					}
					if u.EOF {
						if u.Error != "" {
							streamErr = errors.New("log stream ended: " + u.Error)
						}
						return false
					}
					if asJSON {
						return true
					}
					line := strings.TrimSuffix(u.Text, "\n")
					if u.Time != "" {
						line = u.Time + " " + line
					}
					if u.Stream == "stderr" {
						fmt.Fprintln(stderr, line)
					} else {
						fmt.Fprintln(stdout, line)
					}
					return true
				})
			if err != nil {
				return explainError(err)
			}

			select {
			case <-sub.Done():
				return streamErr
			case <-ctx.Done():
				uctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = sub.Close(uctx)
				return nil
			case <-c.Done():
				return errors.New("connection to the agent was lost")
			}
		},
	}
	cmd.Flags().IntVar(&tail, "tail", 100, "number of past lines to show first")
	cmd.Flags().StringVar(&since, "since", "", "only lines after this RFC 3339 time or duration ago (e.g. 10m)")
	cmd.Flags().BoolVar(&timestamps, "timestamps", false, "prefix each line with its timestamp")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print each raw update as a JSON line")
	return cmd
}

// parseSince turns an RFC 3339 timestamp or a duration-ago into an RFC 3339
// timestamp relative to now.
func parseSince(v string, now time.Time) (string, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d).UTC().Format(time.RFC3339Nano), nil
	}
	return "", fmt.Errorf("invalid --since %q: use an RFC 3339 time or a duration like 10m", v)
}
