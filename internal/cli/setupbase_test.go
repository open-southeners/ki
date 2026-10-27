package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/config"
)

func TestSetupBase(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*config.Config)
		hostnames []string
		wantBase  string
		wantFP    string
		ipHost    bool // base host is the machine's own IPv4 (or the loopback fallback)
	}{
		{"public_url wins", func(c *config.Config) {
			c.Panel.PublicURL = "https://ki.example.com"
			c.Edge.Domain = "other.example.com"
		}, []string{"cp.example.com"}, "https://ki.example.com", "", false},
		{"edge domain", func(c *config.Config) { c.Edge.Domain = "agent.example.com" },
			[]string{"cp.example.com"}, "https://agent.example.com", "", false},
		{"installed hostname", func(*config.Config) {}, []string{"cp.example.com"}, "https://cp.example.com", "", false},
		{"custom https port", func(c *config.Config) { c.Edge.Domain = "a.example.com"; c.Edge.HTTPSPort = 8443 },
			nil, "https://a.example.com:8443", "", false},
		{"ip with fingerprint", func(*config.Config) {}, nil, "", "ab:cd", true},
		{"edge disabled", func(c *config.Config) { c.Edge.Enabled = false }, nil, "http://127.0.0.1:8091", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Edge.Enabled = true
			tt.mutate(&cfg)
			base, fp := setupBase(&cfg, func() []string { return tt.hostnames }, func() string { return "ab:cd" })()
			if fp != tt.wantFP {
				t.Errorf("fingerprint = %q, want %q", fp, tt.wantFP)
			}
			if tt.ipHost {
				if !strings.HasPrefix(base, "https://") || strings.ContainsAny(base[len("https://"):], "abcdefghijklmnopqrstuvwxyz") {
					t.Errorf("base = %q, want an https IP URL", base)
				}
				return
			}
			if base != tt.wantBase {
				t.Errorf("base = %q, want %q", base, tt.wantBase)
			}
		})
	}
}

func TestEdgeAPIPort(t *testing.T) {
	cfg := config.Default()
	if got := edgeAPIPort(&cfg); got != 8092 {
		t.Errorf("edgeAPIPort = %d, want 8092", got)
	}
	cfg.API.EdgeListen = ""
	if got := edgeAPIPort(&cfg); got != 0 {
		t.Errorf("edgeAPIPort(empty) = %d, want 0", got)
	}
}

func TestWarnLegacyEnv(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, nil))
	warnLegacyEnv(l)
	if buf.Len() != 0 {
		t.Fatalf("unexpected warning: %s", buf.String())
	}
	t.Setenv("KI_WEBSOCKET_LISTEN", "127.0.0.1:1")
	warnLegacyEnv(l)
	if out := buf.String(); !strings.Contains(out, "KI_WEBSOCKET_LISTEN") || !strings.Contains(out, "KI_API_") {
		t.Fatalf("warning = %q", out)
	}
}
