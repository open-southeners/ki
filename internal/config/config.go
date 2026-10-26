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
	"path/filepath"
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

// API configures the agent's HTTP/WebSocket listeners. Both bind loopback
// only: Listen serves local scripts directly, EdgeListen is the single
// upstream the edge proxy routes to.
type API struct {
	// Listen is the direct loopback host:port.
	Listen string `yaml:"listen"`
	// EdgeListen is the loopback host:port the edge routes to; it may be empty
	// only when the edge is disabled.
	EdgeListen string `yaml:"edge_listen"`
	// PublicTokens allows bearer tokens on /rpc through the edge listener.
	PublicTokens      bool          `yaml:"public_tokens"`
	HeartbeatInterval time.Duration `yaml:"heartbeat_interval"`
	HeartbeatTimeout  time.Duration `yaml:"heartbeat_timeout"`
}

// Panel configures the web panel and its sessions.
type Panel struct {
	// Enabled serves /auth/* and the panel page.
	Enabled bool `yaml:"enabled"`
	// PublicURL is the https origin used to build setup links; empty derives
	// it from the edge domain or host.
	PublicURL string `yaml:"public_url"`
	// SessionIdle expires a session after this much inactivity.
	SessionIdle time.Duration `yaml:"session_idle"`
	// SessionMax is the absolute session lifetime.
	SessionMax time.Duration `yaml:"session_max"`
}

// Monitor configures resource sampling and in-memory history.
type Monitor struct {
	// HostInterval and ContainerInterval are the sampling periods while at
	// least one client is subscribed.
	HostInterval      time.Duration `yaml:"host_interval"`
	ContainerInterval time.Duration `yaml:"container_interval"`
	// History is how far back samples are kept; HistoryResolution is the
	// spacing of the retained samples.
	History           time.Duration `yaml:"history"`
	HistoryResolution time.Duration `yaml:"history_resolution"`
	// DiskPaths are the absolute mount points reported; empty selects the
	// data directory's filesystem.
	DiskPaths []string `yaml:"disk_paths"`
}

func (m Monitor) validate() error {
	if m.HostInterval < time.Second {
		return errors.New("monitor.host_interval must be at least 1s")
	}
	if m.ContainerInterval < 2*time.Second {
		return errors.New("monitor.container_interval must be at least 2s")
	}
	if m.HistoryResolution < time.Second {
		return errors.New("monitor.history_resolution must be at least 1s")
	}
	if m.History > 24*time.Hour {
		return errors.New("monitor.history must not exceed 24h")
	}
	if m.HistoryResolution > m.History {
		return errors.New("monitor.history_resolution must not exceed monitor.history")
	}
	for _, p := range m.DiskPaths {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("monitor.disk_paths entry %q must be an absolute path", p)
		}
	}
	return nil
}

// checkLoopbackAddr validates a loopback host:port and returns the port.
func checkLoopbackAddr(key, addr string) (int, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return 0, fmt.Errorf("%s %q must be host:port", key, addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || !validPort(n) {
		return 0, fmt.Errorf("%s %q has an invalid port", key, addr)
	}
	if !IsLoopbackHost(host) {
		return 0, fmt.Errorf("%s %q must use a loopback host (127.0.0.1, ::1 or localhost)", key, addr)
	}
	return n, nil
}

func (c *Config) validateAPI() error {
	a := c.API
	listenPort, err := checkLoopbackAddr("api.listen", a.Listen)
	if err != nil {
		return err
	}
	if c.Edge.Enabled && a.EdgeListen == "" {
		return errors.New("api.edge_listen must be set when the edge is enabled")
	}
	edgePort := 0
	if a.EdgeListen != "" {
		if edgePort, err = checkLoopbackAddr("api.edge_listen", a.EdgeListen); err != nil {
			return err
		}
		if edgePort == listenPort {
			return errors.New("api.listen and api.edge_listen must use different ports")
		}
	}
	if c.Edge.Enabled {
		hooks := c.Edge.WebhooksPort
		if c.Webhooks.Enabled() {
			if _, p, err := net.SplitHostPort(c.Webhooks.Listen); err == nil {
				if n, err := strconv.Atoi(p); err == nil {
					hooks = n
				}
			}
		}
		if listenPort == hooks || edgePort == hooks {
			return fmt.Errorf("api.listen and api.edge_listen must not use the webhooks port %d", hooks)
		}
	}
	if a.HeartbeatInterval <= 0 {
		return errors.New("api.heartbeat_interval must be positive")
	}
	if a.HeartbeatTimeout <= a.HeartbeatInterval {
		return errors.New("api.heartbeat_timeout must be greater than api.heartbeat_interval")
	}
	return nil
}

func (p Panel) validate() error {
	if p.SessionIdle <= 0 {
		return errors.New("panel.session_idle must be positive")
	}
	if p.SessionMax <= 0 {
		return errors.New("panel.session_max must be positive")
	}
	if p.SessionIdle > p.SessionMax {
		return errors.New("panel.session_idle must not exceed panel.session_max")
	}
	if p.PublicURL == "" {
		return nil
	}
	u, err := url.Parse(p.PublicURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return fmt.Errorf("panel.public_url %q must be an https URL with a host", p.PublicURL)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("panel.public_url %q must not include a path, query or fragment", p.PublicURL)
	}
	return nil
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
	// ConfigDir is the Docker client config directory (DOCKER_CONFIG) deploys
	// run with: registry logins and credential helpers live here. Empty
	// selects <data_dir>/docker.
	ConfigDir string `yaml:"config_dir"`
}

// ResolvedConfigDir is ConfigDir, or <dataDir>/docker when unset.
func (d Docker) ResolvedConfigDir(dataDir string) string {
	if d.ConfigDir != "" {
		return d.ConfigDir
	}
	return filepath.Join(dataDir, "docker")
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
	// Pull is "always" (refresh image-only services before every deploy) or
	// "missing" (only pull images that are absent, when starting).
	Pull string `yaml:"pull"`
}

// Deploys.Pull values.
const (
	PullAlways  = "always"
	PullMissing = "missing"
)

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
	DataDir  string   `yaml:"data_dir"`
	LogLevel string   `yaml:"log_level"`
	Socket   Socket   `yaml:"socket"`
	API      API      `yaml:"api"`
	Panel    Panel    `yaml:"panel"`
	Docker   Docker   `yaml:"docker"`
	Webhooks Webhooks `yaml:"webhooks"`
	Deploys  Deploys  `yaml:"deploys"`
	Edge     Edge     `yaml:"edge"`
	Monitor  Monitor  `yaml:"monitor"`

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
		Deploys:  Deploys{HealthTimeout: 5 * time.Minute, Keep: 50, LogMaxBytes: 1 << 20, Pull: PullAlways},
		API: API{
			Listen:            "127.0.0.1:8091",
			EdgeListen:        "127.0.0.1:8092",
			HeartbeatInterval: 15 * time.Second,
			HeartbeatTimeout:  45 * time.Second,
		},
		Monitor: Monitor{
			HostInterval:      15 * time.Second,
			ContainerInterval: 30 * time.Second,
			History:           time.Hour,
			HistoryResolution: 15 * time.Second,
		},
		Panel: Panel{Enabled: true, SessionIdle: 12 * time.Hour, SessionMax: 7 * 24 * time.Hour},
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
		var probe map[string]any
		if err := yaml.Unmarshal(b, &probe); err == nil {
			if _, ok := probe["websocket"]; ok {
				return nil, fmt.Errorf("parse config %s: websocket: replaced by api (api.listen); bearer tokens are now created with `ki-agent tokens create`", path)
			}
		}
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
	str("KI_DOCKER_CONFIG_DIR", &cfg.Docker.ConfigDir)
	str("KI_DEPLOYS_PULL", &cfg.Deploys.Pull)
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
	str("KI_API_LISTEN", &cfg.API.Listen)
	str("KI_API_EDGE_LISTEN", &cfg.API.EdgeListen)
	for key, dst := range map[string]*bool{
		"KI_API_PUBLIC_TOKENS": &cfg.API.PublicTokens,
		"KI_PANEL_ENABLED":     &cfg.Panel.Enabled,
	} {
		if v, ok := lookup(key); ok {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dst = b
		}
	}
	str("KI_PANEL_PUBLIC_URL", &cfg.Panel.PublicURL)
	for key, dst := range map[string]*time.Duration{
		"KI_API_HEARTBEAT_INTERVAL": &cfg.API.HeartbeatInterval,
		"KI_API_HEARTBEAT_TIMEOUT":  &cfg.API.HeartbeatTimeout,
		"KI_PANEL_SESSION_IDLE":     &cfg.Panel.SessionIdle,
		"KI_PANEL_SESSION_MAX":      &cfg.Panel.SessionMax,
	} {
		if err := dur(key, dst); err != nil {
			return err
		}
	}
	for key, dst := range map[string]*time.Duration{
		"KI_MONITOR_HOST_INTERVAL":      &cfg.Monitor.HostInterval,
		"KI_MONITOR_CONTAINER_INTERVAL": &cfg.Monitor.ContainerInterval,
		"KI_MONITOR_HISTORY":            &cfg.Monitor.History,
		"KI_MONITOR_HISTORY_RESOLUTION": &cfg.Monitor.HistoryResolution,
	} {
		if err := dur(key, dst); err != nil {
			return err
		}
	}
	if v, ok := lookup("KI_MONITOR_DISK_PATHS"); ok {
		cfg.Monitor.DiskPaths = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Monitor.DiskPaths = append(cfg.Monitor.DiskPaths, p)
			}
		}
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

// Validate checks the configuration for unusable values.
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
	if c.Deploys.Pull != PullAlways && c.Deploys.Pull != PullMissing {
		return fmt.Errorf("deploys.pull %q must be %q or %q", c.Deploys.Pull, PullAlways, PullMissing)
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
	if err := c.validateAPI(); err != nil {
		return err
	}
	if err := c.Monitor.validate(); err != nil {
		return err
	}
	return c.Panel.validate()
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
