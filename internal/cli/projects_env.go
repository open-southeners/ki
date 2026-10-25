package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newProjectsEnvCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Manage a project's environment variables",
		Long: `Manage the environment variables the agent writes to a project's .env file at
deploy time, where Compose reads them for interpolation and env_file. Values are
stored encrypted and are never shown again.`,
	}
	cmd.AddCommand(
		newEnvListCmd(f),
		newEnvSetCmd(f),
		newEnvImportCmd(f),
		newEnvUnsetCmd(f),
	)
	return cmd
}

// envNames is the result of projects.env.set and projects.env.unset.
type envNames struct {
	Names []string `json:"names"`
}

func newEnvListCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list <slug>",
		Short: "List a project's environment variable names",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.env.list", map[string]any{"slug": args[0]}, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var res struct {
				Vars []struct {
					Name      string `json:"name"`
					UpdatedAt string `json:"updated_at"`
				} `json:"vars"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(res.Vars) == 0 {
				fmt.Fprintln(w, "No environment variables.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tUPDATED")
			for _, v := range res.Vars {
				fmt.Fprintf(tw, "%s\t%s\n", v.Name, v.UpdatedAt)
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// printEnvChange prints the outcome of a set, import or unset.
func printEnvChange(cmd *cobra.Command, raw json.RawMessage, asJSON bool, verb string, n int) error {
	if asJSON {
		return printJSON(cmd.OutOrStdout(), raw)
	}
	var res envNames
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s %d variable(s); the project now has %d. Redeploy to apply.\n", verb, n, len(res.Names))
	return nil
}

func newEnvSetCmd(f *flags) *cobra.Command {
	var (
		stdin  bool
		file   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "set <slug> <NAME> (--value-stdin | --value-file <path>)",
		Short: "Set one environment variable",
		Long: `Set one environment variable. The value is read from standard input
(--value-stdin) or from a file (--value-file); one trailing newline is removed.
There is deliberately no plain value argument: it would leak through the
process list and shell history.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw []byte
			var err error
			switch {
			case stdin:
				raw, err = io.ReadAll(cmd.InOrStdin())
			case file != "":
				raw, err = os.ReadFile(file)
			default:
				return errors.New("pass the value with --value-stdin or --value-file")
			}
			if err != nil {
				return fmt.Errorf("read value: %w", err)
			}
			value := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
			if value == "" {
				return errors.New("value is empty")
			}
			params := map[string]any{"slug": args[0], "vars": map[string]string{args[1]: value}}
			var res json.RawMessage
			if err := callOnce(cmd, f, "projects.env.set", params, &res); err != nil {
				return err
			}
			return printEnvChange(cmd, res, asJSON, "Set", 1)
		},
	}
	cmd.Flags().BoolVar(&stdin, "value-stdin", false, "read the value from standard input")
	cmd.Flags().StringVar(&file, "value-file", "", "read the value from `file`")
	cmd.MarkFlagsMutuallyExclusive("value-stdin", "value-file")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newEnvImportCmd(f *flags) *cobra.Command {
	var (
		file   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "import <slug> --file <dotenv>",
		Short: "Set environment variables from a dotenv file",
		Long: `Set environment variables from a dotenv file ("-" reads standard input).
Lines are NAME=VALUE with an optional "export " prefix; blank lines and lines
starting with # are skipped, and values may be single- or double-quoted.
Existing variables not in the file are left alone.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var r io.Reader
			if file == "-" {
				r = cmd.InOrStdin()
			} else {
				fh, err := os.Open(file)
				if err != nil {
					return fmt.Errorf("read dotenv file: %w", err)
				}
				defer fh.Close()
				r = fh
			}
			vars, err := parseDotenv(r)
			if err != nil {
				return err
			}
			if len(vars) == 0 {
				return errors.New("no variables found in the file")
			}
			params := map[string]any{"slug": args[0], "vars": vars}
			var res json.RawMessage
			if err := callOnce(cmd, f, "projects.env.set", params, &res); err != nil {
				return err
			}
			return printEnvChange(cmd, res, asJSON, "Set", len(vars))
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "dotenv `file` to import (- for standard input)")
	_ = cmd.MarkFlagRequired("file")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newEnvUnsetCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "unset <slug> <NAME>...",
		Short: "Remove environment variables",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"slug": args[0], "names": args[1:]}
			var res json.RawMessage
			if err := callOnce(cmd, f, "projects.env.unset", params, &res); err != nil {
				return err
			}
			return printEnvChange(cmd, res, asJSON, "Unset", len(args)-1)
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// parseDotenv reads NAME=VALUE lines. Later duplicates win. Errors name the
// line number but never echo a value.
func parseDotenv(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if n == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(rest)
		}
		name, val, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("dotenv line %d: expected NAME=VALUE", n)
		}
		v, err := dotenvValue(strings.TrimSpace(val))
		if err != nil {
			return nil, fmt.Errorf("dotenv line %d: %w", n, err)
		}
		out[name] = v
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read dotenv file: %w", err)
	}
	return out, nil
}

// dotenvValue unquotes one value. Double quotes understand \n, \r, \t, \" and
// \\; single quotes are literal; unquoted values end at an unescaped " #".
func dotenvValue(v string) (string, error) {
	if v == "" {
		return "", nil
	}
	switch v[0] {
	case '\'':
		end := strings.IndexByte(v[1:], '\'')
		if end < 0 {
			return "", errors.New("unterminated single quote")
		}
		return v[1 : 1+end], nil
	case '"':
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			switch c := v[i]; c {
			case '"':
				return b.String(), nil
			case '\\':
				i++
				if i >= len(v) {
					return "", errors.New("unterminated double quote")
				}
				switch e := v[i]; e {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				case '"', '\\', '$', '\'':
					b.WriteByte(e)
				default:
					b.WriteByte('\\')
					b.WriteByte(e)
				}
			default:
				b.WriteByte(c)
			}
		}
		return "", errors.New("unterminated double quote")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v), nil
}
