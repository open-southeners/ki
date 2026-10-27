package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

// imageFlags are the inputs of "projects create --image".
type imageFlags struct {
	image, service, path string
	port                 int
	domains, volumes     []string
	env                  []string
}

// parseEnvPairs turns KEY=VALUE arguments into an object; the first "=" splits
// and a repeated key is an error.
func parseEnvPairs(pairs []string) (map[string]string, error) {
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid --env %q: want KEY=VALUE", p)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("--env %s given more than once", k)
		}
		out[k] = v
	}
	return out, nil
}

// createImageProject implements "projects create <slug> --image".
func createImageProject(cmd *cobra.Command, f *flags, slug string, in *imageFlags, asJSON bool) error {
	for _, n := range []string{"slug", "branch", "tags", "compose", "token-stdin", "token-file", "token-user"} {
		if cmd.Flags().Changed(n) {
			return fmt.Errorf("--%s cannot be combined with --image: an image project has no Git repository", n)
		}
	}
	if strings.Contains(slug, "://") || strings.Contains(slug, "@") {
		return fmt.Errorf("%q looks like a Git URL: with --image pass the project slug", slug)
	}
	fl := cmd.Flags()
	if len(in.domains) > 0 && !fl.Changed("port") {
		return errors.New("--domain needs --port")
	}
	img := map[string]any{"image": in.image}
	if in.service != "" {
		img["service"] = in.service
	}
	if fl.Changed("port") {
		if in.port < 1 || in.port > 65535 {
			return fmt.Errorf("invalid port %d: want 1-65535", in.port)
		}
		img["port"] = in.port
	}
	if len(in.domains) > 0 {
		img["domains"] = in.domains
	}
	if in.path != "" {
		img["path"] = in.path
	}
	if len(in.volumes) > 0 {
		img["volumes"] = in.volumes
	}
	if len(in.env) > 0 {
		env, err := parseEnvPairs(in.env)
		if err != nil {
			return err
		}
		img["env"] = env
	}
	var raw json.RawMessage
	if err := callOnce(cmd, f, "projects.create", map[string]any{"slug": slug, "image": img}, &raw); err != nil {
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

func newProjectsImageCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Release a new image of a Compose project",
	}
	cmd.AddCommand(newImageSetCmd(f))
	return cmd
}

func newImageSetCmd(f *flags) *cobra.Command {
	var (
		image, service, note string
		doDeploy             bool
		detach, asJSON       bool
	)
	cmd := &cobra.Command{
		Use:   "set <slug> <tag> | <slug> --image <ref> [--service name] [--note text] [--deploy]",
		Short: "Store a new Compose version with a different image",
		Long: `Store a new version of a Compose project with another image: either a new tag
or digest for the current image (positional argument) or a full reference
(--image). Everything else in the Compose file is kept. Tags such as "latest"
are refused. --service picks the service when several have an image.

With --deploy the new version is deployed and the deploy log followed until it
finishes (--detach returns at once).`,
		Example: `  ki agent projects image set web 1.4.2 --deploy
  ki agent projects image set web --image registry.example.com/team/app:1.4.2`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if detach && !doDeploy {
				return errors.New("--detach needs --deploy")
			}
			if (len(args) == 2) == (image != "") {
				return errors.New("give exactly one of a TAG argument or --image")
			}
			params := map[string]any{"slug": args[0]}
			if image != "" {
				params["image"] = image
			} else {
				params["tag"] = args[1]
			}
			if service != "" {
				params["service"] = service
			}
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
			if err := c.Call(ctx, "projects.image.set", params, &raw); err != nil {
				return composeError(err)
			}
			var res struct {
				Version int     `json:"version"`
				Image   string  `json:"image"`
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
				fmt.Fprintf(w, "Stored compose v%d for %s: %s.\n", res.Version, args[0], res.Image)
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
	cmd.Flags().StringVar(&image, "image", "", "full image `reference` to use instead of a tag argument")
	cmd.Flags().StringVar(&service, "service", "", "`service` to change (default: the only one with an image)")
	cmd.Flags().StringVar(&note, "note", "", "free-text note recorded with the version")
	cmd.Flags().BoolVar(&doDeploy, "deploy", false, "deploy the new version and follow the deploy log")
	cmd.Flags().BoolVar(&detach, "detach", false, "with --deploy, do not wait for the deploy to finish")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result (without following) or each raw log update as JSON")
	return cmd
}
