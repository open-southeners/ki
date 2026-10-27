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

// project mirrors the Project object of the projects.* methods.
type project struct {
	Slug           string  `json:"slug"`
	Source         string  `json:"source"`
	ComposeVersion int     `json:"compose_version"`
	RepoURL        string  `json:"repo_url"`
	Trigger        trigger `json:"trigger"`
	DefaultBranch  string  `json:"default_branch"`
	ComposeFile    string  `json:"compose_file"`
	Auth           struct {
		Method    string `json:"method"`
		PublicKey string `json:"public_key"`
		TokenUser string `json:"token_user"`
	} `json:"auth"`
	Webhook struct {
		Path string `json:"path"`
	} `json:"webhook"`
	Status       string `json:"status"`
	StatusDetail string `json:"status_detail"`
	Commit       string `json:"commit"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`

	Exposure         []exposure `json:"exposure"`
	ExposureWarnings []string   `json:"exposure_warnings"`
}

type trigger struct {
	Branch     string `json:"branch"`
	TagPattern string `json:"tag_pattern"`
}

// String renders the trigger as "branch main" or "tags v*". An ssh_key project
// created without --branch/--tags has an empty trigger until it is verified.
func (t trigger) String() string {
	switch {
	case t.Branch != "":
		return "branch " + t.Branch
	case t.TagPattern != "":
		return "tags " + t.TagPattern
	}
	return "default branch (resolved on verify)"
}

// isCompose reports whether the project was created from a stored Compose file.
func (p *project) isCompose() bool { return p.Source == "compose" }

// sourceName is the SOURCE shown for a project; agents that predate Compose
// projects omit the field and only know Git.
func (p *project) sourceName() string {
	if p.Source == "" {
		return "git"
	}
	return p.Source
}

// projectResult is the result of projects.create and projects.update.
type projectResult struct {
	Project       project `json:"project"`
	WebhookSecret string  `json:"webhook_secret"`
}

// stdinIsTerminal reports whether r is an interactive terminal. Tests replace it.
var stdinIsTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func newProjectsCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "Manage projects deployed from Git repositories",
	}
	cmd.AddCommand(
		newProjectsCreateCmd(f),
		newProjectsListCmd(f),
		newProjectsGetCmd(f),
		newProjectsVerifyCmd(f),
		newProjectsUpdateCmd(f),
		newProjectsDeleteCmd(f),
		newProjectsEnvCmd(f),
		newProjectsComposeCmd(f),
		newProjectsExposeCmd(f),
		newProjectsUnexposeCmd(f),
	)
	return cmd
}

// tokenFlags are the credential inputs shared by create and update. There is
// deliberately no plain --token flag: it would leak through the process list
// and shell history.
type tokenFlags struct {
	stdin bool
	file  string
	user  string
}

func (t *tokenFlags) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&t.stdin, "token-stdin", false, "read the HTTPS access token from standard input")
	cmd.Flags().StringVar(&t.file, "token-file", "", "read the HTTPS access token from `file`")
	cmd.Flags().StringVar(&t.user, "token-user", "", "username to send with the token (default x-access-token)")
	cmd.MarkFlagsMutuallyExclusive("token-stdin", "token-file")
}

// read returns the token, or "" if none was requested.
func (t *tokenFlags) read(cmd *cobra.Command) (string, error) {
	var raw []byte
	var err error
	switch {
	case t.stdin:
		raw, err = io.ReadAll(cmd.InOrStdin())
	case t.file != "":
		raw, err = os.ReadFile(t.file)
	default:
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", errors.New("token is empty")
	}
	return tok, nil
}

func newProjectsCreateCmd(f *flags) *cobra.Command {
	var (
		slug, branch, tags, compose, composeFile string
		tok                                      tokenFlags
		asJSON                                   bool
	)
	cmd := &cobra.Command{
		Use:   "create <git-url> | <slug> --compose-file <file>",
		Short: "Create a project from a Git repository or a Compose file",
		Long: `Create a project from a Git repository.

For SSH URLs the agent generates a deploy key; add its public key to the
repository as a read-only deploy key and run "ki-agent projects verify".
For private HTTPS repositories pass an access token on standard input
(--token-stdin) or from a file (--token-file).

With --compose-file the argument is the project slug and the project is
created from that Compose file ("-" reads standard input) instead of a Git
repository; see "ki-agent projects compose".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if composeFile != "" {
				return createComposeProject(cmd, f, args[0], composeFile, asJSON)
			}
			params := map[string]any{"repo_url": args[0]}
			if slug != "" {
				params["slug"] = slug
			}
			if branch != "" {
				params["branch"] = branch
			}
			if tags != "" {
				params["tag_pattern"] = tags
			}
			if compose != "" {
				params["compose_file"] = compose
			}
			token, err := tok.read(cmd)
			if err != nil {
				return err
			}
			if token != "" {
				params["token"] = token
			} else if tok.user != "" {
				return errors.New("--token-user needs --token-stdin or --token-file")
			}
			if tok.user != "" {
				params["token_user"] = tok.user
			}

			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.create", params, &raw); err != nil {
				return err
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
			printSecrets(w, &res.Project, res.WebhookSecret)
			return nil
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "project identifier (default: derived from the repository name)")
	cmd.Flags().StringVar(&branch, "branch", "", "deploy pushes to this branch (default: the repository's default branch)")
	cmd.Flags().StringVar(&tags, "tags", "", "deploy tags matching this `glob` instead of a branch")
	cmd.Flags().StringVar(&compose, "compose", "", "Compose file `path` inside the repository (default: auto-detect)")
	cmd.Flags().StringVar(&composeFile, "compose-file", "", "create a Compose project from this `file` (- for standard input); the argument is the slug")
	cmd.MarkFlagsMutuallyExclusive("branch", "tags")
	tok.register(cmd)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newProjectsListCmd(f *flags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.list", nil, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var list []project
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintln(w, "No projects.")
				return nil
			}
			tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SLUG\tSOURCE\tSTATUS\tTRIGGER\tREPOSITORY")
			for _, p := range list {
				if p.isCompose() {
					fmt.Fprintf(tw, "%s\t%s\t%s\t-\tcompose v%d\n", p.Slug, p.sourceName(), p.Status, p.ComposeVersion)
					continue
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.Slug, p.sourceName(), p.Status, p.Trigger, p.RepoURL)
			}
			return tw.Flush()
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// newProjectGetter builds "get" and "verify", which differ only in method and heading.
func newProjectGetter(f *flags, use, short, method string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   use + " <slug>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var raw json.RawMessage
			if err := callOnce(cmd, f, method, map[string]any{"slug": args[0]}, &raw); err != nil {
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
			printProject(w, &p)
			printExposure(w, &p)
			if p.Status == "pending_access" {
				printDeployKey(w, &p)
			}
			return nil
		},
	}
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newProjectsGetCmd(f *flags) *cobra.Command {
	return newProjectGetter(f, "get", "Show a project", "projects.get")
}

func newProjectsVerifyCmd(f *flags) *cobra.Command {
	return newProjectGetter(f, "verify", "Check repository access and prepare the checkout", "projects.verify")
}

func newProjectsUpdateCmd(f *flags) *cobra.Command {
	var (
		branch, tags, compose   string
		tok                     tokenFlags
		rotateSecret, rotateKey bool
		asJSON                  bool
	)
	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Change a project's trigger, Compose file or credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{"slug": args[0]}
			fl := cmd.Flags()
			if fl.Changed("branch") {
				params["branch"] = branch
			}
			if fl.Changed("tags") {
				params["tag_pattern"] = tags
			}
			if fl.Changed("compose") {
				params["compose_file"] = compose
			}
			token, err := tok.read(cmd)
			if err != nil {
				return err
			}
			if token != "" {
				params["token"] = token
			}
			if fl.Changed("token-user") {
				params["token_user"] = tok.user
			}
			if rotateSecret {
				params["rotate_webhook_secret"] = true
			}
			if rotateKey {
				params["rotate_deploy_key"] = true
			}
			if len(params) == 1 {
				return errors.New("nothing to update: pass at least one flag")
			}

			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.update", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var res projectResult
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Project %s updated (%s)\n\n", res.Project.Slug, res.Project.Status)
			printProject(w, &res.Project)
			if res.WebhookSecret != "" || rotateKey {
				printSecrets(w, &res.Project, res.WebhookSecret)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "deploy pushes to this branch")
	cmd.Flags().StringVar(&tags, "tags", "", "deploy tags matching this `glob`")
	cmd.Flags().StringVar(&compose, "compose", "", "Compose file `path` inside the repository")
	cmd.Flags().BoolVar(&rotateSecret, "rotate-webhook-secret", false, "generate a new webhook secret (shown once)")
	cmd.Flags().BoolVar(&rotateKey, "rotate-deploy-key", false, "generate a new SSH deploy key (access must be re-granted)")
	cmd.MarkFlagsMutuallyExclusive("branch", "tags")
	tok.register(cmd)
	addJSONFlag(cmd, &asJSON)
	return cmd
}

func newProjectsDeleteCmd(f *flags) *cobra.Command {
	var (
		volumes, yes, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "delete <slug>",
		Short: "Delete a project, its containers and its checkout",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			if !yes {
				if err := confirmSlug(cmd, slug, volumes); err != nil {
					return err
				}
			}
			params := map[string]any{"slug": slug}
			if volumes {
				params["volumes"] = true
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.delete", params, &raw); err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), raw)
			}
			var res struct {
				Containers, Networks, Volumes int
				KeptVolumes                   []string `json:"kept_volumes"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted project %s: removed %d containers, %d networks, %d volumes\n",
				slug, res.Containers, res.Networks, res.Volumes)
			if n := len(res.KeptVolumes); n > 0 {
				w := cmd.OutOrStdout()
				fmt.Fprintf(w, "Kept %d anonymous volume(s) that may hold data:\n", n)
				for _, v := range res.KeptVolumes {
					fmt.Fprintf(w, "  %s\n", v)
				}
				fmt.Fprintf(w, "Remove them with: docker volume rm %s\n", strings.Join(res.KeptVolumes, " "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&volumes, "volumes", false, "also remove the project's volumes (data is lost)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	addJSONFlag(cmd, &asJSON)
	return cmd
}

// confirmSlug asks the user to type slug. It fails when stdin is not a terminal.
func confirmSlug(cmd *cobra.Command, slug string, volumes bool) error {
	in := cmd.InOrStdin()
	if !stdinIsTerminal(in) {
		return errors.New("refusing to delete without confirmation: pass --yes when not running interactively")
	}
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "This removes project %s, its containers and its checkout", slug)
	if volumes {
		fmt.Fprint(w, " and its volumes (data is lost)")
	}
	fmt.Fprintf(w, ".\nType the project slug to confirm: ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("aborted: no confirmation given")
	}
	if strings.TrimSpace(line) != slug {
		return errors.New("aborted: slug did not match")
	}
	return nil
}

// printProject writes the aligned key/value view of a project.
func printProject(w io.Writer, p *project) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) {
		if v != "" {
			fmt.Fprintf(tw, "%s\t%s\n", k, v)
		}
	}
	row("Slug", p.Slug)
	row("Source", p.sourceName())
	if p.isCompose() {
		row("Compose", fmt.Sprintf("v%d", p.ComposeVersion))
		status := p.Status
		if p.StatusDetail != "" {
			status += " - " + p.StatusDetail
		}
		row("Status", status)
		row("Created", p.CreatedAt)
		row("Updated", p.UpdatedAt)
		_ = tw.Flush()
		return
	}
	row("Repository", p.RepoURL)
	row("Trigger", p.Trigger.String())
	row("Default branch", p.DefaultBranch)
	row("Compose file", p.ComposeFile)
	auth := p.Auth.Method
	if p.Auth.Method == "https_token" && p.Auth.TokenUser != "" {
		auth += " (user " + p.Auth.TokenUser + ")"
	}
	row("Auth", auth)
	status := p.Status
	if p.StatusDetail != "" {
		status += " - " + p.StatusDetail
	}
	row("Status", status)
	if len(p.Commit) > 12 {
		row("Commit", p.Commit[:12])
	} else {
		row("Commit", p.Commit)
	}
	row("Webhook path", p.Webhook.Path)
	row("Created", p.CreatedAt)
	row("Updated", p.UpdatedAt)
	_ = tw.Flush()
}

// printDeployKey explains how to grant the agent access with an SSH key.
func printDeployKey(w io.Writer, p *project) {
	if p.Auth.Method != "ssh_key" || p.Auth.PublicKey == "" {
		return
	}
	fmt.Fprintf(w, "\nDeploy key:\n  %s\n\n", p.Auth.PublicKey)
	fmt.Fprintf(w, "Add it as a read-only deploy key, then run `ki-agent projects verify %s`.\n", p.Slug)
}

// printSecrets writes the one-time webhook secret and the deploy key
// instructions after create or a rotation.
func printSecrets(w io.Writer, p *project, secret string) {
	if secret != "" {
		fmt.Fprintf(w, "\nWebhook (the secret is shown only once; store it now):\n")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "  Path\t%s\n", p.Webhook.Path)
		fmt.Fprintf(tw, "  Secret\t%s\n", secret)
		fmt.Fprintf(tw, "  Content type\tapplication/json\n")
		_ = tw.Flush()
		fmt.Fprintln(w, "  GitHub: send the \"push\" event. GitLab: send \"Push events\" and \"Tag push events\".")
	}
	printDeployKey(w, p)
}
