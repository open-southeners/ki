package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/config"
)

func TestEffectiveWebhooksListen(t *testing.T) {
	tests := []struct {
		name       string
		edge       bool
		listen     string
		tlsFiles   bool
		want       string
		wantPort   int
		wantWarn   string
		wantNoWarn bool
	}{
		{name: "edge off, no listener", want: "", wantPort: 8090},
		{name: "edge off keeps explicit", listen: "127.0.0.1:9000", want: "127.0.0.1:9000", wantPort: 8090},
		{name: "edge on, implicit loopback", edge: true, want: "127.0.0.1:8090", wantPort: 8090},
		{name: "edge on, explicit same port", edge: true, listen: "127.0.0.1:8090", want: "127.0.0.1:8090", wantPort: 8090, wantNoWarn: true},
		{name: "edge on, explicit other port retargets", edge: true, listen: "127.0.0.1:9000", want: "127.0.0.1:9000", wantPort: 9000, wantNoWarn: true},
		{name: "edge on, wildcard is reachable", edge: true, listen: "0.0.0.0:9001", want: "0.0.0.0:9001", wantPort: 9001, wantNoWarn: true},
		{name: "edge on, other address warns", edge: true, listen: "10.1.2.3:9002", want: "10.1.2.3:9002", wantPort: 9002, wantWarn: "neither loopback nor a wildcard"},
		{name: "edge on, tls warns", edge: true, listen: "127.0.0.1:9003", tlsFiles: true, want: "127.0.0.1:9003", wantPort: 9003, wantWarn: "plain HTTP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{
				Edge:     config.Edge{Enabled: tt.edge, WebhooksPort: 8090},
				Webhooks: config.Webhooks{Listen: tt.listen},
			}
			if tt.tlsFiles {
				cfg.Webhooks.TLSCertFile, cfg.Webhooks.TLSKeyFile = "c.pem", "k.pem"
			}
			var buf bytes.Buffer
			got := effectiveWebhooksListen(cfg, slog.New(slog.NewTextHandler(&buf, nil)))
			if got != tt.want {
				t.Errorf("listen = %q, want %q", got, tt.want)
			}
			if cfg.Edge.WebhooksPort != tt.wantPort {
				t.Errorf("edge webhooks port = %d, want %d", cfg.Edge.WebhooksPort, tt.wantPort)
			}
			if tt.wantWarn != "" && !strings.Contains(buf.String(), tt.wantWarn) {
				t.Errorf("log %q lacks %q", buf.String(), tt.wantWarn)
			}
			if tt.wantNoWarn && strings.Contains(buf.String(), "level=WARN") {
				t.Errorf("unexpected warning: %s", buf.String())
			}
		})
	}
}
