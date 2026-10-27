package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-southeners/ki/internal/edge/certs"
)

func newEdgeProxyConfigCmd(f *flags) *cobra.Command {
	var (
		format  string
		domains []string
	)
	cmd := &cobra.Command{
		Use:   "proxy-config [--format nginx|caddy] [--domain d ...]",
		Short: "Print front-proxy config that forwards domains to the edge (behind mode)",
		Long: `Prints a ready-to-paste block per domain for the web server in front of the
edge (edge.mode: behind). Each block proxies to the edge's loopback listener,
preserving the Host header, X-Forwarded-For/Proto and WebSocket upgrades.

Without --domain the domains are every effective project exposure domain plus
the server hostnames. TLS stays on the front server (for example Forge's
certificate); the agent never writes web server configuration, you add the
output yourself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "nginx" && format != "caddy" {
				return fmt.Errorf("--format must be nginx or caddy")
			}
			for _, d := range domains {
				if err := certs.ValidHostname(d); err != nil {
					return fmt.Errorf("--domain: %w", err)
				}
			}
			var raw json.RawMessage
			if err := callOnce(cmd, f, "edge.status", nil, &raw); err != nil {
				return err
			}
			var st edgeStatus
			if err := json.Unmarshal(raw, &st); err != nil {
				return fmt.Errorf("decode result: %w", err)
			}
			if !st.Enabled {
				return errors.New("the edge is disabled (edge.enabled: false)")
			}
			if st.Mode != "behind" || st.Listen == "" {
				return errors.New("proxy-config is only for edge.mode: behind; the edge currently owns the public ports (front mode)")
			}
			list := dedupe(domains)
			if len(list) == 0 {
				var err error
				if list, err = derivedProxyDomains(cmd, f, st); err != nil {
					return err
				}
			}
			if len(list) == 0 {
				return errors.New("no domains: pass --domain or expose a project service / set edge.domain first")
			}
			w := cmd.OutOrStdout()
			if format == "caddy" {
				writeCaddy(w, st.Listen, list)
			} else {
				writeNginx(w, st.Listen, list)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&format, "format", "nginx", "output format: nginx or caddy")
	cmd.Flags().StringArrayVar(&domains, "domain", nil, "domain to forward (repeatable); default: all exposure domains and server hostnames")
	return cmd
}

// derivedProxyDomains collects the server hostnames and every effective
// exposure domain of all projects.
func derivedProxyDomains(cmd *cobra.Command, f *flags, st edgeStatus) ([]string, error) {
	list := append([]string(nil), st.ServerHostnames...)
	var rawList json.RawMessage
	if err := callOnce(cmd, f, "projects.list", nil, &rawList); err != nil {
		return nil, err
	}
	var projects []project
	if err := json.Unmarshal(rawList, &projects); err != nil {
		return nil, fmt.Errorf("decode result: %w", err)
	}
	for _, p := range projects {
		exp := p.Exposure
		if len(exp) == 0 {
			// The list may omit the exposure; the single-project view has it.
			var raw json.RawMessage
			if err := callOnce(cmd, f, "projects.get", map[string]any{"slug": p.Slug}, &raw); err != nil {
				return nil, err
			}
			var full project
			if err := json.Unmarshal(raw, &full); err != nil {
				return nil, fmt.Errorf("decode result: %w", err)
			}
			exp = full.Exposure
		}
		for _, e := range exp {
			list = append(list, e.Domains...)
		}
	}
	return dedupe(list), nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range in {
		d = strings.ToLower(d)
		if d != "" && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func writeNginx(w io.Writer, listen string, domains []string) {
	fmt.Fprintf(w, `# Front-proxy config for the ki-agent edge (edge.mode: behind).
# The edge listens on %[1]s in plain HTTP; TLS stays on this front server
# (keep using its certificate, for example the one Forge manages). The agent
# never writes nginx config: paste the blocks below yourself.
#
# WebSocket upgrades need this map once in the http context. If your nginx
# (or Forge's global config) does not already define $connection_upgrade, add:
#
#   map $http_upgrade $connection_upgrade {
#       default upgrade;
#       ''      close;
#   }
#
# Per site, add this location to the existing server block for the domain
# (replacing its current "location /" / app handler):
`, listen)
	for _, d := range domains {
		fmt.Fprintf(w, `
# --- %[1]s
location / {
    proxy_pass http://%[2]s;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection $connection_upgrade;
    proxy_read_timeout 300s;
}
`, d, listen)
	}
}

func writeCaddy(w io.Writer, listen string, domains []string) {
	fmt.Fprintf(w, `# Front-proxy config for the ki-agent edge (edge.mode: behind).
# The edge listens on %s in plain HTTP; TLS stays on this front server
# (Caddy obtains or serves the certificates). The agent never writes Caddy
# config: paste the sites below into your Caddyfile yourself. Caddy preserves
# Host, sets X-Forwarded-For/Proto and proxies WebSocket upgrades by default.
`, listen)
	for _, d := range domains {
		fmt.Fprintf(w, "\n%s {\n    reverse_proxy %s\n}\n", d, listen)
	}
}
