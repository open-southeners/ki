package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/version"
)

func newEnrollCmd(f *flags) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "enroll [URL]",
		Short: "Connect this agent to a control plane by approving a code in a browser",
		Long: `Start a device-flow enrollment: the agent prints a URL and a short code, you
approve the code in a browser, and the agent stores the credentials and connects
to the control plane without a restart. Ctrl-C cancels the enrollment.

URL is the control plane's enrollment address; it may be omitted when the
binary was built with a default.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			params := map[string]any{}
			if len(args) == 1 {
				params["url"] = args[0]
			} else if version.EnrollURL == "" {
				return errors.New("no control plane URL: pass it as an argument, for example `ki agent enroll https://cp.example.com`")
			}
			if force {
				params["force"] = true
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			// Closing the connection cancels the enrollment on the agent.
			defer c.Close()

			var start struct {
				UserCode                string `json:"user_code"`
				VerificationURI         string `json:"verification_uri"`
				VerificationURIComplete string `json:"verification_uri_complete"`
				ExpiresAt               string `json:"expires_at"`
			}
			if err := c.Call(ctx, "enrollment.start", params, &start); err != nil {
				return explainEnrollError(err)
			}
			w := cmd.OutOrStdout()
			uri := start.VerificationURI
			if start.VerificationURIComplete != "" {
				uri = start.VerificationURIComplete
			}
			fmt.Fprintf(w, "Open %s and confirm the code %s\n", uri, start.UserCode)
			if start.ExpiresAt != "" {
				fmt.Fprintf(w, "The code expires at %s. Waiting for approval (Ctrl-C to cancel)...\n", start.ExpiresAt)
			}

			var done struct {
				ControlPlaneURL string `json:"control_plane_url"`
			}
			if err := c.Call(ctx, "enrollment.wait", nil, &done); err != nil {
				if ctx.Err() != nil {
					return errors.New("enrollment cancelled")
				}
				return explainEnrollError(err)
			}
			fmt.Fprintf(w, "Enrolled. The agent is connecting to %s.\n", done.ControlPlaneURL)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "enroll again even if the agent is already enrolled")
	return cmd
}

// explainEnrollError is explainError with wording for the enroll flags.
func explainEnrollError(err error) error {
	var re *rpc.Error
	if errors.As(err, &re) && re.Code == rpc.CodeAlreadyExists && strings.HasPrefix(re.Message, "already enrolled") {
		return errors.New("already enrolled: run `ki agent unenroll` first or pass --force")
	}
	return explainError(err)
}

func newUnenrollCmd(f *flags) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "unenroll",
		Short: "Forget the enrollment and disconnect from the enrolled control plane",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				if err := confirmYes(cmd, "This deletes the stored control plane credentials and disconnects the agent."); err != nil {
					return err
				}
			}
			c, err := connect(cmd, f)
			if err != nil {
				return err
			}
			defer c.Close()
			if err := c.Call(cmd.Context(), "enrollment.remove", nil, nil); err != nil {
				return explainError(err)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, "Enrollment removed.")
			// Best effort: tell the user when the config's control plane took over.
			var info struct {
				ControlPlane struct {
					State string `json:"state"`
					URL   string `json:"url"`
				} `json:"control_plane"`
			}
			if c.Call(cmd.Context(), "agent.info", nil, &info) == nil && info.ControlPlane.State != "disabled" {
				fmt.Fprintf(w, "The agent now uses the control plane from its config file: %s\n", info.ControlPlane.URL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// confirmYes asks a yes/no question. It fails when stdin is not a terminal.
func confirmYes(cmd *cobra.Command, what string) error {
	in := cmd.InOrStdin()
	if !stdinIsTerminal(in) {
		return errors.New("refusing to continue without confirmation: pass --yes when not running interactively")
	}
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "%s Continue? [y/N] ", what)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return errors.New("aborted: no confirmation given")
	}
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		return errors.New("aborted")
	}
	return nil
}
