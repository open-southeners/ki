package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki/internal/rpc"
)

// maxComposeSize is the largest Compose file the CLI sends; the agent enforces
// the same limit.
const maxComposeSize = 256 << 10

// readComposeFile reads a Compose file from path, or from standard input for "-".
func readComposeFile(cmd *cobra.Command, path string) (string, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxComposeSize+1))
	} else {
		var fh *os.File
		if fh, err = os.Open(path); err == nil {
			defer fh.Close()
			raw, err = io.ReadAll(io.LimitReader(fh, maxComposeSize+1))
		}
	}
	if err != nil {
		return "", fmt.Errorf("read Compose file: %w", err)
	}
	if len(raw) > maxComposeSize {
		return "", fmt.Errorf("the Compose file is larger than %d KiB", maxComposeSize>>10)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", errors.New("the Compose file is empty")
	}
	return string(raw), nil
}

// createComposeProject implements "projects create <slug> --compose-file".
func createComposeProject(cmd *cobra.Command, f *flags, slug, file string, asJSON bool) error {
	for _, n := range []string{"slug", "branch", "tags", "compose", "token-stdin", "token-file", "token-user"} {
		if cmd.Flags().Changed(n) {
			return fmt.Errorf("--%s cannot be combined with --compose-file: a Compose project has no Git repository", n)
		}
	}
	if strings.Contains(slug, "://") || strings.Contains(slug, "@") {
		return fmt.Errorf("%q looks like a Git URL: with --compose-file pass the project slug", slug)
	}
	content, err := readComposeFile(cmd, file)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := callOnce(cmd, f, "projects.create", map[string]any{"slug": slug, "compose": content}, &raw); err != nil {
		return composeError(err)
	}
	if asJSON {
		return printJSON(cmd.OutOrStdout(), raw)
	}
	var res projectResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "Project %s created (%s)\n\n", res.Project.Slug, res.Project.Status)
	printProject(w, &res.Project)
	return nil
}

// composeError renders a rejected Compose file: InvalidParams carrying
// data.violations becomes the message plus a violations table.
func composeError(err error) error {
	var re *rpc.Error
	if errors.As(err, &re) && re.Code == rpc.CodeInvalidParams {
		var d struct {
			Violations []violation `json:"violations"`
		}
		if json.Unmarshal(re.Data, &d) == nil && len(d.Violations) > 0 {
			var b strings.Builder
			writeViolations(&b, "", d.Violations)
			return fmt.Errorf("%s\n%s", re.Message, strings.TrimRight(b.String(), "\n"))
		}
	}
	return explainError(err)
}

func newProjectsComposeCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "compose",
		Short: "Manage the stored Compose file of a Compose project",
	}
	cmd.AddCommand(newComposeGetCmd(f), newComposeSetCmd(f), newComposeHistoryCmd(f))
	return cmd
}

func newComposeGetCmd(f *flags) *cobra.Command {
	var (
		version int
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "get <slug> [--version N]",
		Short: "Print a project's stored Compose file",
		Long:  "Print the stored Compose file of a Compose project to standard output, exactly as stored. Without --version the latest version is shown.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"slug": args[0]}
			if cmd.Flags().Changed("version") {
				if version <= 0 {
					return fmt.Errorf("invalid version %d: want a positive number", version)
				}
				params["version"] = version
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.compose.get", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var res struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			_, err := io.WriteString(cmd.OutOrStdout(), res.Content)
			return err
		},
	}
	cmd.Flags().IntVar(&version, "version", 0, "show this `version` (default: the latest)")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newComposeSetCmd(f *flags) *cobra.Command {
	var (
		file, note     string
		doDeploy       bool
		detach, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "set <slug> --file <file> [--note text] [--deploy]",
		Short: "Store a new version of a project's Compose file",
		Long: `Store a new version of a Compose project's Compose file ("-" reads standard
input). The file is validated against the deploy policy first; violations are
listed and nothing is stored. With --deploy the new version is deployed and the
deploy log followed until it finishes (--detach returns at once).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if detach && !doDeploy {
				return errors.New("--detach needs --deploy")
			}
			content, err := readComposeFile(cmd, file)
			if err != nil {
				return err
			}
			params := map[string]any{"slug": args[0], "compose": content}
			if note != "" {
				params["note"] = note
			}
			if doDeploy {
				params["deploy"] = true
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			defer c.Close()

			var raw json.RawMessage
			if err := c.Call(ctx, "projects.compose.set", params, &raw); err != nil {
				return composeError(err)
			}
			var res struct {
				Version int     `json:"version"`
				Deploy  *deploy `json:"deploy"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			follow := doDeploy && !detach && res.Deploy != nil
			if asJSON && !follow {
				return printJSON(w, raw)
			}
			if !asJSON {
				fmt.Fprintf(w, "Stored compose v%d for %s.\n", res.Version, args[0])
				if d := res.Deploy; d != nil {
					fmt.Fprintf(w, "Deploy %d %s for project %s (ref %s)\n", d.ID, d.Status, d.Project, d.Ref)
				}
			}
			if follow {
				return followDeploy(ctx, cmd, c, res.Deploy.ID, true, asJSON)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "Compose `file` to store (- for standard input)")
	cmd.Flags().StringVar(&note, "note", "", "free-text note recorded with the version")
	cmd.Flags().BoolVar(&doDeploy, "deploy", false, "deploy the new version and follow the deploy log")
	cmd.Flags().BoolVar(&detach, "detach", false, "with --deploy, do not wait for the deploy to finish")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result (without following) or each raw log update as JSON")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newComposeHistoryCmd(f *flags) *cobra.Command {
	var (
		limit  int
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "history <slug> [--limit N]",
		Short: "List the stored versions of a project's Compose file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"slug": args[0]}
			if cmd.Flags().Changed("limit") {
				if limit <= 0 {
					return fmt.Errorf("invalid limit %d: want a positive number", limit)
				}
				params["limit"] = limit
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.compose.history", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var list []struct {
				Version   int    `json:"version"`
				CreatedAt string `json:"created_at"`
				CreatedBy string `json:"created_by"`
				Note      string `json:"note"`
				Deployed  bool   `json:"deployed"`
			}
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "No versions.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "VERSION\tCREATED\tBY\tNOTE\tDEPLOYED")
			for _, v := range list {
				deployed := "no"
				if v.Deployed {
					deployed = "yes"
				}
				note := v.Note
				if note == "" {
					note = "-"
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", v.Version, v.CreatedAt, v.CreatedBy, note, deployed)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "show at most this many `versions` (newest first)")
	addJSONFlag(cmd, &asJSON)
	return cmd
}
