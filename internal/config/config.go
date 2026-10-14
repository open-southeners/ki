// Package config loads the ki-agent configuration from a YAML file with
// KI_* environment variable overrides.
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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

// Docker configures the container engine connection.
type Docker struct {
	// Host is the engine endpoint (unix:// or tcp://). The agent starts even
	// when the engine is down; container methods fail until it is reachable.
	Host string `yaml:"host"`
}

// Config is the full agent configuration.
type Config struct {
	DataDir   string    `yaml:"data_dir"`
	LogLevel  string    `yaml:"log_level"`
	Socket    Socket    `yaml:"socket"`
	WebSocket WebSocket `yaml:"websocket"`
	Docker    Docker    `yaml:"docker"`
}

// Default returns the configuration used when nothing is configured.
func Default() Config {
	return Config{
		DataDir:  "/var/lib/ki-agent",
		LogLevel: "info",
		Socket:   Socket{Path: "/run/ki-agent/ki.sock", Mode: 0o660},
		Docker:   Docker{Host: "unix:///var/run/docker.sock"},
		WebSocket: WebSocket{
			Path:              "/rpc",
			HeartbeatInterval: 15 * time.Second,
			HeartbeatTimeout:  45 * time.Second,
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
	str("KI_WEBSOCKET_LISTEN", &cfg.WebSocket.Listen)
	str("KI_WEBSOCKET_PATH", &cfg.WebSocket.Path)
	str("KI_WEBSOCKET_TOKEN_FILE", &cfg.WebSocket.TokenFile)
	if err := dur("KI_WEBSOCKET_HEARTBEAT_INTERVAL", &cfg.WebSocket.HeartbeatInterval); err != nil {
		return err
	}
	return dur("KI_WEBSOCKET_HEARTBEAT_TIMEOUT", &cfg.WebSocket.HeartbeatTimeout)
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
