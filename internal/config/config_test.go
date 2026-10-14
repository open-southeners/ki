package config

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noEnv(string) (string, bool) { return "", false }

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func write(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsWhenMissing(t *testing.T) {
	cfg, err := LoadWithEnv(filepath.Join(t.TempDir(), "nope.yaml"), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if *cfg != Default() {
		t.Fatalf("got %+v, want defaults %+v", *cfg, Default())
	}
	d := Default()
	if d.DataDir != "/var/lib/ki-agent" || d.Socket.Mode != 0o660 || d.WebSocket.Path != "/rpc" ||
		d.WebSocket.HeartbeatInterval != 15*time.Second || d.WebSocket.HeartbeatTimeout != 45*time.Second ||
		d.Docker.Host != "unix:///var/run/docker.sock" {
		t.Fatalf("unexpected defaults: %+v", d)
	}
}

func TestLoadFileAndEnv(t *testing.T) {
	dir := t.TempDir()
	tok := write(t, dir, "token", "  s3cret\n")
	yml := write(t, dir, "c.yaml", `
data_dir: /tmp/data
log_level: debug
socket:
  path: /tmp/ki.sock
  mode: 0600
websocket:
  listen: 127.0.0.1:9000
  token_file: `+tok+`
  heartbeat_interval: 5s
  heartbeat_timeout: 20s
docker:
  host: tcp://dind:2375
`)
	tests := []struct {
		name  string
		env   map[string]string
		check func(*Config) bool
	}{
		{"file values", nil, func(c *Config) bool {
			return c.DataDir == "/tmp/data" && c.LogLevel == "debug" && c.Socket.Mode == 0o600 &&
				c.WebSocket.Listen == "127.0.0.1:9000" && c.WebSocket.HeartbeatInterval == 5*time.Second &&
				c.WebSocket.Path == "/rpc" && c.Docker.Host == "tcp://dind:2375"
		}},
		{"env overrides file", map[string]string{
			"KI_DATA_DIR": "/srv/x", "KI_LOG_LEVEL": "warn", "KI_SOCKET_MODE": "0640",
			"KI_SOCKET_PATH": "/run/x.sock", "KI_WEBSOCKET_PATH": "/ws",
			"KI_DOCKER_HOST":                  "unix:///run/docker.sock",
			"KI_WEBSOCKET_HEARTBEAT_INTERVAL": "1s", "KI_WEBSOCKET_HEARTBEAT_TIMEOUT": "3s",
		}, func(c *Config) bool {
			return c.DataDir == "/srv/x" && c.LogLevel == "warn" && c.Socket.Mode == 0o640 &&
				c.Socket.Path == "/run/x.sock" && c.WebSocket.Path == "/ws" &&
				c.Docker.Host == "unix:///run/docker.sock" &&
				c.WebSocket.HeartbeatInterval == time.Second && c.WebSocket.HeartbeatTimeout == 3*time.Second
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadWithEnv(yml, envMap(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(cfg) {
				t.Fatalf("unexpected config: %+v", *cfg)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	empty := write(t, dir, "empty-token", " \n")
	good := write(t, dir, "token", "abc")
	tests := []struct {
		name string
		yaml string
		env  map[string]string
		want string
	}{
		{"bad yaml", "data_dir: [", nil, "parse config"},
		{"unknown key", "bogus: 1\n", nil, "parse config"},
		{"bad log level", "log_level: loud\n", nil, "invalid log_level"},
		{"bad mode", "socket:\n  mode: 0999\n", nil, "invalid file mode"},
		{"bad env mode", "", map[string]string{"KI_SOCKET_MODE": "rw"}, "KI_SOCKET_MODE"},
		{"bad env duration", "", map[string]string{"KI_WEBSOCKET_HEARTBEAT_TIMEOUT": "x"}, "KI_WEBSOCKET_HEARTBEAT_TIMEOUT"},
		{"empty docker host", "docker:\n  host: \"\"\n", nil, "docker.host"},
		{"ws without token file", "websocket:\n  listen: :9000\n", nil, "token_file is not set"},
		{"ws missing token file", "websocket:\n  listen: :9000\n  token_file: " + filepath.Join(dir, "none") + "\n", nil, "read websocket token file"},
		{"ws empty token", "websocket:\n  listen: :9000\n  token_file: " + empty + "\n", nil, "is empty"},
		{"ws bad path", "websocket:\n  listen: :9000\n  path: rpc\n  token_file: " + good + "\n", nil, "must start with /"},
		{"ws timeout <= interval", "websocket:\n  listen: :9000\n  token_file: " + good + "\n  heartbeat_interval: 10s\n  heartbeat_timeout: 10s\n", nil, "heartbeat_timeout"},
		{"ws via env no token", "", map[string]string{"KI_WEBSOCKET_LISTEN": ":9000"}, "token_file is not set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := write(t, dir, "c.yaml", tt.yaml)
			_, err := LoadWithEnv(p, envMap(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestToken(t *testing.T) {
	dir := t.TempDir()
	w := WebSocket{TokenFile: write(t, dir, "t", "\n tok123 \n")}
	got, err := w.Token()
	if err != nil || got != "tok123" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
}

func TestParseLogLevel(t *testing.T) {
	for _, s := range []string{"debug", "INFO", "warn", "warning", "error"} {
		if _, err := ParseLogLevel(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := ParseLogLevel("nope"); err == nil {
		t.Error("expected error")
	}
}

func TestControlPlaneLoadAndEnv(t *testing.T) {
	dir := t.TempDir()
	tok := write(t, dir, "token", " abc \n")
	yml := write(t, dir, "c.yaml", `
control_plane:
  url: wss://cp.example.com/agent
  token_file: `+tok+`
  reconnect_min: 2s
  reconnect_max: 30s
`)
	cfg, err := LoadWithEnv(yml, envMap(map[string]string{"KI_CONTROL_PLANE_HEARTBEAT_INTERVAL": "5s"}))
	if err != nil {
		t.Fatal(err)
	}
	cp := cfg.ControlPlane
	if !cp.Enabled() || cp.URL != "wss://cp.example.com/agent" || cp.ReconnectMin != 2*time.Second ||
		cp.ReconnectMax != 30*time.Second || cp.HeartbeatInterval != 5*time.Second ||
		cp.HeartbeatTimeout != 45*time.Second {
		t.Fatalf("unexpected control plane config: %+v", cp)
	}
	if got, err := cp.Token(); err != nil || got != "abc" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
	tc, err := cp.TLSConfig()
	if err != nil || tc.MinVersion != tls.VersionTLS12 || tc.RootCAs == nil {
		t.Fatalf("TLSConfig() = %+v, %v", tc, err)
	}

	env := envMap(map[string]string{
		"KI_CONTROL_PLANE_URL":        "ws://localhost:1/x",
		"KI_CONTROL_PLANE_TOKEN_FILE": tok,
	})
	cfg, err = LoadWithEnv(filepath.Join(dir, "none.yaml"), env)
	if err != nil || cfg.ControlPlane.URL != "ws://localhost:1/x" {
		t.Fatalf("env load: %+v, %v", cfg, err)
	}
}

func TestControlPlaneDisabledByDefault(t *testing.T) {
	cfg, err := LoadWithEnv(filepath.Join(t.TempDir(), "none.yaml"), noEnv)
	if err != nil || cfg.ControlPlane.Enabled() {
		t.Fatalf("cfg = %+v, err = %v", cfg.ControlPlane, err)
	}
}

func TestControlPlaneValidation(t *testing.T) {
	dir := t.TempDir()
	tok := write(t, dir, "token", "abc")
	empty := write(t, dir, "empty", "\n")
	badCA := write(t, dir, "bad.pem", "not pem")
	base := func() Config {
		c := Default()
		c.ControlPlane.URL = "wss://cp.example.com/agent"
		c.ControlPlane.TokenFile = tok
		return c
	}
	tests := []struct {
		name string
		mod  func(*Config)
		want string // empty = valid
	}{
		{"ok wss", func(*Config) {}, ""},
		{"ok ws localhost", func(c *Config) { c.ControlPlane.URL = "ws://localhost:8080/a" }, ""},
		{"ok ws v4 loopback", func(c *Config) { c.ControlPlane.URL = "ws://127.0.0.1:8080/a" }, ""},
		{"ok ws v6 loopback", func(c *Config) { c.ControlPlane.URL = "ws://[::1]:8080/a" }, ""},
		{"ws non-loopback", func(c *Config) { c.ControlPlane.URL = "ws://cp.example.com/a" }, "wss://"},
		{"ws private ip", func(c *Config) { c.ControlPlane.URL = "ws://10.0.0.1/a" }, "wss://"},
		{"http scheme", func(c *Config) { c.ControlPlane.URL = "https://cp.example.com/a" }, "scheme"},
		{"no host", func(c *Config) { c.ControlPlane.URL = "wss:///a" }, "host"},
		{"no token file", func(c *Config) { c.ControlPlane.TokenFile = "" }, "token_file"},
		{"empty token", func(c *Config) { c.ControlPlane.TokenFile = empty }, "empty"},
		{"min > max", func(c *Config) { c.ControlPlane.ReconnectMin = time.Hour }, "reconnect_min"},
		{"timeout <= interval", func(c *Config) { c.ControlPlane.HeartbeatTimeout = c.ControlPlane.HeartbeatInterval }, "heartbeat_timeout"},
		{"missing ca", func(c *Config) { c.ControlPlane.CAFile = filepath.Join(dir, "nope.pem") }, "ca file"},
		{"garbage ca", func(c *Config) { c.ControlPlane.CAFile = badCA }, "no PEM"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mod(&c)
			err := c.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want containing %q", err, tt.want)
			}
		})
	}
}
