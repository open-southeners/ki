package cli

import (
	"net"
	"net/url"
	"strconv"

	"github.com/open-southeners/ki-agent/internal/config"
)

// setupBase returns the function the accounts service uses to build setup
// links: the base URL and, when it is an IP literal served by the edge's
// self-signed certificate, that certificate's SHA-256 fingerprint.
//
// Precedence: panel.public_url, edge.domain, the first installed
// control-plane hostname (hostnames lists them), the host's first non-loopback
// IPv4 address (with the fingerprint). Without the edge the panel is only
// reachable directly, so the link points at api.listen (a localhost origin,
// which browsers treat as secure for the session cookie).
func setupBase(cfg *config.Config, hostnames func() []string, fingerprint func() string) func() (string, string) {
	return func() (string, string) {
		if cfg.Panel.PublicURL != "" {
			return cfg.Panel.PublicURL, ""
		}
		if !cfg.Edge.Enabled {
			return "http://" + cfg.API.Listen, ""
		}
		hostPort := func(host string) string {
			// Behind mode: the front proxy serves the public https port.
			if cfg.Edge.Mode != config.EdgeModeBehind && cfg.Edge.HTTPSPort != 443 {
				return net.JoinHostPort(host, strconv.Itoa(cfg.Edge.HTTPSPort))
			}
			return host
		}
		if cfg.Edge.Domain != "" {
			return (&url.URL{Scheme: "https", Host: hostPort(cfg.Edge.Domain)}).String(), ""
		}
		if hostnames != nil {
			if hs := hostnames(); len(hs) > 0 {
				return (&url.URL{Scheme: "https", Host: hostPort(hs[0])}).String(), ""
			}
		}
		host := firstNonLoopbackIPv4()
		if host == "" {
			host = "127.0.0.1"
		}
		u := &url.URL{Scheme: "https", Host: hostPort(host)}
		if fingerprint != nil {
			return u.String(), fingerprint()
		}
		return u.String(), ""
	}
}

// firstNonLoopbackIPv4 returns the first non-loopback, non-link-local IPv4
// address of the host, or "" when there is none.
func firstNonLoopbackIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				return ip4.String()
			}
		}
	}
	return ""
}
