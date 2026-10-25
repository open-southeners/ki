// Package config loads the ki-agent configuration from a YAML file with
// KI_* environment variable overrides.
package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/open-southeners/ki-agent/internal/edge/certs"
)

// DefaultPath is where the config file is read from when --config is not given.
const DefaultPath = "/etc/ki-agent/config.yaml"

// FileMode is an os.FileMode written in config as octal digits (for example
// 0660 or "660").
type FileMode os.FileMode

// UnmarshalYAML parses the value as octal digits.
func (m *FileMode) UnmarshalYAML(n *yaml.Node) error {
	v, err := parseMode(n.Value)
	if err != nil {
		return err
	}
	*m = v
	return nil
}

func parseMode(s string) (FileMode, error) {
	u, err := strconv.ParseUint(strings.TrimSpace(s), 8, 32)
	if err != nil || u > 0o777 {
		return 0, fmt.Errorf("invalid file mode %q: want octal such as 0660", s)
	}
	return FileMode(u), nil
}

// Socket configures the local Unix socket.
type Socket struct {
	Path string   `yaml:"path"`
	Mode FileMode `yaml:"mode"`
}

// WebSocket configures the optional authenticated WebSocket listener.
type WebSocket struct {
	// Listen is the host:port to listen on; empty disables the listener.
	Listen string `yaml:"listen"`
	// Path is the HTTP path the JSON-RPC endpoint is served on.
	Path string `yaml:"path"`
	// TokenFile holds the bearer token; required when Listen is set.
	TokenFile         string        `yaml:"token_file"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat_timeout"`
}

// Enabled reports whether the WebSocket listener is configured.
func (w WebSocket) Enabled() bool { return w.Listen != "" }

// Token reads the bearer token from TokenFile and trims surrounding whitespace.
func (w WebSocket) Token() (string, error) {
	if w.TokenFile == "" {
		return "", errors.New("websocket.token_file is not set")
	}
	b, err := os.ReadFile(w.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read websocket token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("websocket token file %s is empty", w.TokenFile)
	}
	return t, nil
}

// ControlPlane configures the optional outbound connection to a control plane.
type ControlPlane struct {
	// URL is the ws:// or wss:// endpoint to connect to; empty disables the
	// connection. ws:// is only accepted for loopback hosts.
	URL string `yaml:"url"`
	// TokenFile holds the bearer token; required when URL is set.
	TokenFile string `yaml:"token_file"`
	// CAFile is an optional PEM bundle trusted in addition to the system roots.
	CAFile            string        `yaml:"ca_file"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat_timeout"`
	ReconnectMin      time.Duration `yaml:"reconnect_min"`
	ReconnectMax      time.Duration `yaml:"reconnect_max"`
}

// Enabled reports whether the outbound connection is configured.
func (c ControlPlane) Enabled() bool { return c.URL != "" }

// Token reads the bearer token from TokenFile and trims surrounding whitespace.
func (c ControlPlane) Token() (string, error) {
	if c.TokenFile == "" {
		return "", errors.New("control_plane.token_file is not set")
	}
	b, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read control plane token file: %w", err)
	}
	t := strings.TrimSpace(string(b))
	if t == "" {
		return "", fmt.Errorf("control plane token file %s is empty", c.TokenFile)
	}
	return t, nil
}

// TLSConfig returns the client TLS settings: the system roots plus CAFile (if
// set), TLS 1.2 or newer.
func (c ControlPlane) TLSConfig() (*tls.Config, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read control plane ca file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("control plane ca file %s contains no PEM certificates", c.CAFile)
		}
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// CheckControlPlaneURL reports whether raw is an acceptable control_plane.url:
// wss://, or ws:// only for loopback hosts, and it must include a host.
func CheckControlPlaneURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("control_plane.url is not a valid URL")
	}
	switch u.Scheme {
	case "wss":
	case "ws":
		if !IsLoopbackHost(u.Hostname()) {
			return fmt.Errorf("control_plane.url must use wss:// unless the host is loopback (got ws:// to %q)", u.Hostname())
		}
	default:
		return fmt.Errorf("control_plane.url scheme %q is not supported: want wss:// (or ws:// for loopback)", u.Scheme)
	}
	if u.Hostname() == "" {
		return errors.New("control_plane.url must include a host")
	}
	return nil
}

func (c ControlPlane) validate() error {
	if err := CheckControlPlaneURL(c.URL); err != nil {
		return err
	}
	if c.HeartbeatInterval <= 0 {
		return errors.New("control_plane.heartbeat_interval must be positive")
	}
	if c.HeartbeatTimeout <= c.HeartbeatInterval {
		return errors.New("control_plane.heartbeat_timeout must be greater than control_plane.heartbeat_interval")
	}
	if c.ReconnectMin <= 0 {
		return errors.New("control_plane.reconnect_min must be positive")
	}
	if c.ReconnectMin > c.ReconnectMax {
		return errors.New("control_plane.reconnect_min must not exceed control_plane.reconnect_max")
	}
	if _, err := c.Token(); err != nil {
		return fmt.Errorf("control plane enabled but %w", err)
	}
	if _, err := c.TLSConfig(); err != nil {
		return err
	}
	return nil
}

// IsLoopbackHost reports whether h is "localhost" or a loopback IP address.
func IsLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// Docker configures the container engine connection.
type Docker struct {
	// Host is the engine endpoint (unix:// or tcp://). The agent starts even
	// when the engine is down; container methods fail until it is reachable.
	Host string `yaml:"host"`
	// CLI is the docker executable (with the compose v2 plugin) used for
	// deploys; a bare name is looked up in PATH.
	CLI string `yaml:"cli"`
}

// Webhooks configures the optional inbound webhook listener that triggers
// deploys.
type Webhooks struct {
	// Listen is the host:port to listen on; empty disables the listener.
	Listen string `yaml:"listen"`
	// TLSCertFile and TLSKeyFile enable HTTPS; both are required when Listen
	// is not a loopback address.
	TLSCertFile string `yaml:"tls_cert_file"`
	TLSKeyFile  string `yaml:"tls_key_file"`
	// MaxBodyBytes caps the size of an accepted webhook request body.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// Enabled reports whether the webhook listener is configured.
func (w Webhooks) Enabled() bool { return w.Listen != "" }

// TLSEnabled reports whether the listener serves HTTPS.
func (w Webhooks) TLSEnabled() bool { return w.TLSCertFile != "" && w.TLSKeyFile != "" }

func (w Webhooks) validate() error {
	host, port, err := net.SplitHostPort(w.Listen)
	if err != nil || port == "" {
		return fmt.Errorf("webhooks.listen %q must be host:port", w.Listen)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("webhooks.listen %q has an invalid port", w.Listen)
	}
	if (w.TLSCertFile == "") != (w.TLSKeyFile == "") {
		return errors.New("webhooks.tls_cert_file and webhooks.tls_key_file must be set together")
	}
	if !w.TLSEnabled() && !IsLoopbackHost(host) {
		return fmt.Errorf("webhooks.listen %q is not loopback: set webhooks.tls_cert_file and webhooks.tls_key_file", w.Listen)
	}
	if w.MaxBodyBytes <= 0 {
		return errors.New("webhooks.max_body_bytes must be positive")
	}
	return nil
}

// Deploys configures deploy execution and history retention.
type Deploys struct {
	// HealthTimeout is how long a deploy waits for services to become healthy.
	HealthTimeout time.Duration `yaml:"health_timeout"`
	// Keep is the number of deploys retained per project.
	Keep int `yaml:"keep"`
	// LogMaxBytes caps the stored log size of a single deploy.
	LogMaxBytes int64 `yaml:"log_max_bytes"`
}

// Edge configures the managed reverse proxy that routes project domains.
type Edge struct {
	// Enabled turns the managed edge proxy on.
	Enabled bool `yaml:"enabled"`
	// Image is the proxy container image; empty selects the pinned default.
	Image string `yaml:"image"`
	// HTTPPort and HTTPSPort are the host ports the proxy publishes.
	HTTPPort  int `yaml:"http_port"`
	HTTPSPort int `yaml:"https_port"`
	// Domain is the standalone server domain, served with ACME certificates.
	Domain string `yaml:"domain"`
	// ACMEEmail is the optional ACME account contact.
	ACMEEmail string `yaml:"acme_email"`
	// WebhooksPort is the loopback port the webhook listener uses when
	// webhooks.listen is empty.
	WebhooksPort int `yaml:"webhooks_port"`
}

func validPort(n int) bool { return n >= 1 && n <= 65535 }

func (e Edge) validate() error {
	for _, p := range []struct {
		key string
		n   int
	}{{"http_port", e.HTTPPort}, {"https_port", e.HTTPSPort}, {"webhooks_port", e.WebhooksPort}} {
		if !validPort(p.n) {
			return fmt.Errorf("edge.%s %d must be between 1 and 65535", p.key, p.n)
		}
	}
	if e.HTTPPort == e.HTTPSPort || e.HTTPPort == e.WebhooksPort || e.HTTPSPort == e.WebhooksPort {
		return errors.New("edge.http_port, edge.https_port and edge.webhooks_port must be distinct")
	}
	if e.Domain != "" {
		if err := certs.ValidHostname(e.Domain); err != nil {
			return fmt.Errorf("edge.domain: %w", err)
		}
	}
	if e.ACMEEmail != "" && (!strings.Contains(e.ACMEEmail, "@") || strings.ContainsAny(e.ACMEEmail, " \t\r\n")) {
		return fmt.Errorf("edge.acme_email %q must contain @ and no spaces", e.ACMEEmail)
	}
	return nil
}

// Config is the full agent configuration.
type Config struct {
	DataDir   string    `yaml:"data_dir"`
	LogLevel  string    `yaml:"log_level"`
	Socket    Socket    `yaml:"socket"`
	WebSocket WebSocket `yaml:"websocket"`
	Docker    Docker    `yaml:"docker"`
	Webhooks  Webhooks  `yaml:"webhooks"`
	Deploys   Deploys   `yaml:"deploys"`
	Edge      Edge      `yaml:"edge"`

	ControlPlane ControlPlane `yaml:"control_plane"`
}

// Default returns the configuration used when nothing is configured.
func Default() Config {
	return Config{
		DataDir:  "/var/lib/ki-agent",
		LogLevel: "info",
		Socket:   Socket{Path: "/run/ki-agent/ki.sock", Mode: 0o660},
		Docker:   Docker{Host: "unix:///var/run/docker.sock", CLI: "docker"},
		Webhooks: Webhooks{MaxBodyBytes: 5 << 20},
		Edge:     Edge{Enabled: true, HTTPPort: 80, HTTPSPort: 443, WebhooksPort: 8090},
		Deploys:  Deploys{HealthTimeout: 5 * time.Minute, Keep: 50, LogMaxBytes: 1 << 20},
		WebSocket: WebSocket{
			Path:              "/rpc",
			HeartbeatInterval: 15 * time.Second,
			HeartbeatTimeout:  45 * time.Second,
		},
		ControlPlane: ControlPlane{
			HeartbeatInterval: 15 * time.Second,
			HeartbeatTimeout:  45 * time.Second,
			ReconnectMin:      time.Second,
			ReconnectMax:      time.Minute,
		},
	}
}

// Load reads the YAML file at path (a missing file yields the defaults),
// applies KI_* environment overrides and validates the result.
func Load(path string) (*Config, error) {
	return LoadWithEnv(path, os.LookupEnv)
}

// LoadWithEnv is Load with an injectable environment lookup.
func LoadWithEnv(path string, lookup func(string) (string, bool)) (*Config, error) {
	cfg := Default()

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := yaml.NewDecoder(strings.NewReader(string(b)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("parse config %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist):
		// Defaults only.
	default:
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	if err := applyEnv(&cfg, lookup); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func applyEnv(cfg *Config, lookup func(string) (string, bool)) error {
	str := func(key string, dst *string) {
		if v, ok := lookup(key); ok {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) error {
		if v, ok := lookup(key); ok {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = d
		}
		return nil
	}
	num := func(key string, dst *int64) error {
		if v, ok := lookup(key); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = n
		}
		return nil
	}

	str("KI_DATA_DIR", &cfg.DataDir)
	str("KI_LOG_LEVEL", &cfg.LogLevel)
	str("KI_SOCKET_PATH", &cfg.Socket.Path)
	if v, ok := lookup("KI_SOCKET_MODE"); ok {
		m, err := parseMode(v)
		if err != nil {
			return fmt.Errorf("KI_SOCKET_MODE: %w", err)
		}
		cfg.Socket.Mode = m
	}
	str("KI_DOCKER_HOST", &cfg.Docker.Host)
	str("KI_DOCKER_CLI", &cfg.Docker.CLI)
	str("KI_WEBHOOKS_LISTEN", &cfg.Webhooks.Listen)
	str("KI_WEBHOOKS_TLS_CERT_FILE", &cfg.Webhooks.TLSCertFile)
	str("KI_WEBHOOKS_TLS_KEY_FILE", &cfg.Webhooks.TLSKeyFile)
	if err := num("KI_WEBHOOKS_MAX_BODY_BYTES", &cfg.Webhooks.MaxBodyBytes); err != nil {
		return err
	}
	if v, ok := lookup("KI_EDGE_ENABLED"); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("KI_EDGE_ENABLED: %w", err)
		}
		cfg.Edge.Enabled = b
	}
	str("KI_EDGE_IMAGE", &cfg.Edge.Image)
	str("KI_EDGE_DOMAIN", &cfg.Edge.Domain)
	str("KI_EDGE_ACME_EMAIL", &cfg.Edge.ACMEEmail)
	for key, dst := range map[string]*int{
		"KI_EDGE_HTTP_PORT":     &cfg.Edge.HTTPPort,
		"KI_EDGE_HTTPS_PORT":    &cfg.Edge.HTTPSPort,
		"KI_EDGE_WEBHOOKS_PORT": &cfg.Edge.WebhooksPort,
	} {
		n := int64(*dst)
		if err := num(key, &n); err != nil {
			return err
		}
		*dst = int(n)
	}
	if err := dur("KI_DEPLOYS_HEALTH_TIMEOUT", &cfg.Deploys.HealthTimeout); err != nil {
		return err
	}
	keep := int64(cfg.Deploys.Keep)
	if err := num("KI_DEPLOYS_KEEP", &keep); err != nil {
		return err
	}
	cfg.Deploys.Keep = int(keep)
	if err := num("KI_DEPLOYS_LOG_MAX_BYTES", &cfg.Deploys.LogMaxBytes); err != nil {
		return err
	}
	str("KI_WEBSOCKET_LISTEN", &cfg.WebSocket.Listen)
	str("KI_WEBSOCKET_PATH", &cfg.WebSocket.Path)
	str("KI_WEBSOCKET_TOKEN_FILE", &cfg.WebSocket.TokenFile)
	if err := dur("KI_WEBSOCKET_HEARTBEAT_INTERVAL", &cfg.WebSocket.HeartbeatInterval); err != nil {
		return err
	}
	if err := dur("KI_WEBSOCKET_HEARTBEAT_TIMEOUT", &cfg.WebSocket.HeartbeatTimeout); err != nil {
		return err
	}
	str("KI_CONTROL_PLANE_URL", &cfg.ControlPlane.URL)
	str("KI_CONTROL_PLANE_TOKEN_FILE", &cfg.ControlPlane.TokenFile)
	str("KI_CONTROL_PLANE_CA_FILE", &cfg.ControlPlane.CAFile)
	for key, dst := range map[string]*time.Duration{
		"KI_CONTROL_PLANE_HEARTBEAT_INTERVAL": &cfg.ControlPlane.HeartbeatInterval,
		"KI_CONTROL_PLANE_HEARTBEAT_TIMEOUT":  &cfg.ControlPlane.HeartbeatTimeout,
		"KI_CONTROL_PLANE_RECONNECT_MIN":      &cfg.ControlPlane.ReconnectMin,
		"KI_CONTROL_PLANE_RECONNECT_MAX":      &cfg.ControlPlane.ReconnectMax,
	} {
		if err := dur(key, dst); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks the configuration for unusable values. A WebSocket listener
// without a readable, non-empty token file is rejected so the agent never
// serves an unauthenticated network endpoint.
func (c *Config) Validate() error {
	if c.DataDir == "" {
		return errors.New("data_dir must not be empty")
	}
	if _, err := ParseLogLevel(c.LogLevel); err != nil {
		return err
	}
	if c.Socket.Path == "" {
		return errors.New("socket.path must not be empty")
	}
	if c.Docker.Host == "" {
		return errors.New("docker.host must not be empty")
	}
	if c.Docker.CLI == "" {
		return errors.New("docker.cli must not be empty")
	}
	if c.Deploys.HealthTimeout <= 0 {
		return errors.New("deploys.health_timeout must be positive")
	}
	if c.Deploys.Keep <= 0 {
		return errors.New("deploys.keep must be positive")
	}
	if c.Deploys.LogMaxBytes <= 0 {
		return errors.New("deploys.log_max_bytes must be positive")
	}
	if c.Edge.Enabled {
		if err := c.Edge.validate(); err != nil {
			return err
		}
	}
	if c.Webhooks.Enabled() {
		if err := c.Webhooks.validate(); err != nil {
			return err
		}
	}
	if c.ControlPlane.Enabled() {
		if err := c.ControlPlane.validate(); err != nil {
			return err
		}
	}
	if !c.WebSocket.Enabled() {
		return nil
	}
	if !strings.HasPrefix(c.WebSocket.Path, "/") {
		return fmt.Errorf("websocket.path %q must start with /", c.WebSocket.Path)
	}
	if c.WebSocket.HeartbeatInterval <= 0 {
		return errors.New("websocket.heartbeat_interval must be positive")
	}
	if c.WebSocket.HeartbeatTimeout <= c.WebSocket.HeartbeatInterval {
		return errors.New("websocket.heartbeat_timeout must be greater than websocket.heartbeat_interval")
	}
	if _, err := c.WebSocket.Token(); err != nil {
		return fmt.Errorf("websocket enabled but %w", err)
	}
	return nil
}

// ParseLogLevel maps a level name (debug, info, warn, error) to a slog level.
func ParseLogLevel(s string) (slog.Level, error) {
	var l slog.Level
	switch strings.ToLower(s) {
	case "debug":
		l = slog.LevelDebug
	case "info":
		l = slog.LevelInfo
	case "warn", "warning":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		return 0, fmt.Errorf("invalid log_level %q: want debug, info, warn or error", s)
	}
	return l, nil
}
