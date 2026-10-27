package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/mod/semver"

	"github.com/open-southeners/ki/internal/config"
	"github.com/open-southeners/ki/internal/update"
	"github.com/open-southeners/ki/internal/version"
)

// Exit codes of `ki agent update` beyond the generic 1.
const exitRolledBack = 2

// maxNoteLines is how many lines of the release notes are shown before an
// update is confirmed.
const maxNoteLines = 15

// Test hooks.
var (
	// updateGeteuid reports the effective user id; root installs updates.
	updateGeteuid = os.Geteuid
	// updateAPIBaseURL overrides the GitHub API root.
	updateAPIBaseURL = ""
	// updateApplier adjusts the applier before it runs.
	updateApplier = func(*update.Applier) {}
	// updateLayout adjusts the install layout options before they run.
	updateLayout = func(*update.LayoutOptions) {}
)

// exitError makes the process exit with a specific code.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// ExitCode returns the process exit code for an error returned by Execute.
func ExitCode(err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		return ee.code
	}
	return 1
}

func newUpdateCmd(f *flags) *cobra.Command {
	var (
		target      string
		yes         bool
		check       bool
		fromRequest bool
	)
	cmd := &cobra.Command{
		Use:   "update [--check] [--version vX.Y.Z] [--yes]",
		Short: "Check for and install a new agent release",
		Long: `Check for and install a new agent release from GitHub Releases.

As a regular user it shows what the running agent knows about the latest
release (--check asks it to look now). As root (sudo ki agent update) it looks
the release up on GitHub, shows the release notes, asks for confirmation (or
use --yes), verifies the download against the release signing key and its
checksum, swaps the binary, restarts the agent and rolls back if the new
version does not come up.

Exit status: 0 on success or when already up to date, 1 on failure, 2 when the
update was rolled back.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := updateGeteuid() == 0
			switch {
			case fromRequest:
				if !root {
					return errors.New("--from-request must run as root")
				}
				return runUpdateFromRequest(cmd, f)
			case root:
				return runUpdateRoot(cmd, f, target, yes, check)
			default:
				return runUpdateUser(cmd, f, target, check)
			}
		},
	}
	cmd.Flags().StringVar(&target, "version", "", "install this release (vX.Y.Z) instead of the latest")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	cmd.Flags().BoolVar(&check, "check", false, "only look for an update, do not install")
	cmd.Flags().BoolVar(&fromRequest, "from-request", false, "install the release named in the request the agent wrote (used by ki-agent-update.service)")
	_ = cmd.Flags().MarkHidden("from-request")
	return cmd
}

// runUpdateUser shows the agent's view of the latest release.
func runUpdateUser(cmd *cobra.Command, f *flags, target string, check bool) error {
	method := update.MethodStatus
	if check {
		method = update.MethodCheck
	}
	var st update.UpdateStatus
	if err := callOnce(cmd, f, method, nil, &st); err != nil {
		return err
	}
	if err := printUpdateStatus(cmd.OutOrStdout(), st); err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	switch {
	case target != "":
		fmt.Fprintf(w, "\nRun `sudo ki agent update --version %s` to install it.\n", target)
	case st.UpdateAvailable && st.Latest != nil:
		fmt.Fprintf(w, "\nRun `sudo ki agent update` to install %s.\n", st.Latest.Tag)
	}
	return nil
}

func printUpdateStatus(w io.Writer, st update.UpdateStatus) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "Current\t%s\n", st.Current)
	switch {
	case st.Latest != nil:
		latest := st.Latest.Tag
		if st.UpdateAvailable {
			latest += " (update available)"
		}
		fmt.Fprintf(tw, "Latest\t%s\n", latest)
	case !st.CheckEnabled:
		fmt.Fprintln(tw, "Latest\tunknown (update checks are disabled: updates.check)")
	default:
		fmt.Fprintln(tw, "Latest\tunknown")
	}
	fmt.Fprintf(tw, "Channel\t%s\n", st.Channel)
	if st.CheckedAt != nil {
		fmt.Fprintf(tw, "Last checked\t%s\n", st.CheckedAt.Local().Format(time.RFC1123))
	}
	if st.CheckError != nil {
		fmt.Fprintf(tw, "Check error\t%s\n", *st.CheckError)
	}
	if st.State == update.StateRequested {
		fmt.Fprintln(tw, "State\tan update was requested and is being installed")
	}
	if r := st.LastResult; r != nil {
		fmt.Fprintf(tw, "Last update\t%s -> %s: %s\n", r.From, r.To, strings.ReplaceAll(r.Status, "_", " "))
		if r.Error != "" {
			fmt.Fprintf(tw, "\t%s\n", r.Error)
		}
	}
	return tw.Flush()
}

// loadUpdateConfig loads the config for the root commands.
func loadUpdateConfig(f *flags) (*config.Config, error) {
	return config.Load(f.configPath)
}

// newApplier builds an applier for the data directory in cfg.
func newApplier(cmd *cobra.Command, f *flags, cfg *config.Config) *update.Applier {
	a := &update.Applier{
		Current: version.Version,
		DataDir: cfg.DataDir,
		Health:  update.SocketHealth{SocketPath: resolveSocket(f)},
		Log:     slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil)),
	}
	updateApplier(a)
	return a
}

// recordResult writes res to the exchange directory so the agent (and the
// panel) can show it, owned by the owner of the data directory.
func recordResult(cfg *config.Config, res update.Result) error {
	dir := filepath.Join(cfg.DataDir, "update")
	uid, gid := dataOwner(cfg.DataDir)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		if uid >= 0 {
			if err := os.Chown(dir, uid, gid); err != nil {
				return err
			}
		}
	}
	return update.WriteResult(dir, res, uid, gid)
}

// outcome turns an applier result into the command's error and exit code.
func outcome(res update.Result, err error) error {
	switch res.Status {
	case update.ResultSucceeded:
		return nil
	case update.ResultRolledBack:
		return &exitError{code: exitRolledBack, msg: "update to " + res.To + " was rolled back: " + res.Error}
	}
	if err == nil {
		err = errors.New(res.Error)
	}
	return &exitError{code: 1, msg: "update failed: " + err.Error()}
}

// ensureLayout migrates the install to the ki + ki-agent alias layout and
// reports what changed. A failure is reported but never stops the update.
func ensureLayout(cmd *cobra.Command) {
	opts := update.LayoutOptions{Log: slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))}
	updateLayout(&opts)
	sum, err := update.EnsureLayout(cmd.Context(), opts)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not update the install layout: %v\n", err)
	}
	for _, l := range sum.Lines() {
		fmt.Fprintf(cmd.ErrOrStderr(), "layout: %s\n", l)
	}
}

// runUpdateFromRequest is what ki-agent-update.service runs: it installs the
// release the agent asked for and records the result under the request's id.
func runUpdateFromRequest(cmd *cobra.Command, f *flags) error {
	cfg, err := loadUpdateConfig(f)
	if err != nil {
		return err
	}
	req, err := update.ReadRequest(filepath.Join(cfg.DataDir, "update"))
	if err != nil {
		return err
	}
	ensureLayout(cmd)
	cmd.PrintErrf("installing %s, requested by %s\n", req.Version, req.RequestedBy)
	a := newApplier(cmd, f, cfg)
	res, applyErr := a.Apply(cmd.Context(), req.Version)
	res.ID = req.ID
	if err := recordResult(cfg, res); err != nil {
		cmd.PrintErrf("could not record the update result: %v\n", err)
	}
	return outcome(res, applyErr)
}

// runUpdateRoot looks the release up, confirms and installs it.
func runUpdateRoot(cmd *cobra.Command, f *flags, target string, yes, check bool) error {
	cfg, err := loadUpdateConfig(f)
	if err != nil {
		return err
	}
	if !check {
		ensureLayout(cmd)
	}
	if target != "" && !update.ValidTag(target) {
		return fmt.Errorf("invalid --version %q; expected something like v1.2.3", target)
	}
	rel, err := update.LookupRelease(cmd.Context(), update.LookupOptions{
		BaseURL: updateAPIBaseURL, Current: version.Version, Channel: cfg.Updates.Channel, Version: target,
	})
	if err != nil {
		return fmt.Errorf("look up the release: %w", err)
	}

	w := cmd.OutOrStdout()
	current := version.Version
	released := update.ValidTag(update.Canonical(current))
	if released && semver.Compare(rel.Tag, update.Canonical(current)) <= 0 {
		if target != "" {
			return fmt.Errorf("%s is not newer than the running version %s; downgrades are refused", rel.Tag, current)
		}
		fmt.Fprintf(w, "ki %s is up to date.\n", current)
		return nil
	}
	if !released && target == "" {
		fmt.Fprintf(w, "This is a source build (%s); the latest release is %s.\nPass --version %s to replace it with that release.\n",
			current, rel.Tag, rel.Tag)
		return nil
	}
	printReleaseNotes(w, current, rel)
	if check {
		fmt.Fprintf(w, "\nRun `sudo ki agent update` to install %s.\n", rel.Tag)
		return nil
	}

	if !yes {
		ok, err := confirmUpdate(cmd, rel.Tag)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(w, "Cancelled.")
			return nil
		}
	}

	fmt.Fprintf(w, "\nInstalling %s ...\n", rel.Tag)
	a := newApplier(cmd, f, cfg)
	res, applyErr := a.Apply(cmd.Context(), rel.Tag)
	if err := recordResult(cfg, res); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not record the update result: %v\n", err)
	}
	if err := outcome(res, applyErr); err != nil {
		return err
	}
	fmt.Fprintf(w, "Updated ki %s -> %s.\n", res.From, res.To)
	return nil
}

func printReleaseNotes(w io.Writer, current string, rel *update.Release) {
	fmt.Fprintf(w, "Update %s -> %s\n", current, rel.Tag)
	if rel.Name != "" && rel.Name != rel.Tag {
		fmt.Fprintf(w, "%s\n", rel.Name)
	}
	if notes := strings.TrimSpace(rel.Notes); notes != "" {
		lines := strings.Split(strings.ReplaceAll(notes, "\r\n", "\n"), "\n")
		fmt.Fprintln(w)
		for i, l := range lines {
			if i == maxNoteLines {
				fmt.Fprintf(w, "... (%d more lines)\n", len(lines)-i)
				break
			}
			fmt.Fprintln(w, l)
		}
	}
	if rel.URL != "" {
		fmt.Fprintf(w, "\n%s\n", rel.URL)
	}
}

// confirmUpdate asks on the terminal whether to install tag.
func confirmUpdate(cmd *cobra.Command, tag string) (bool, error) {
	in := cmd.InOrStdin()
	if !stdinIsTerminal(in) {
		return false, errors.New("not a terminal: pass --yes to install without asking")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nInstall %s and restart the agent? Containers keep running. [y/N] ", tag)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
