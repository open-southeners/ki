package tunnel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki/internal/config"
	"github.com/open-southeners/ki/internal/runtime"
)

// Tunnel states reported in [Status].
const (
	StateDisabled     = "disabled"
	StateUnconfigured = "unconfigured"
	StateStarting     = "starting"
	StateConnected    = "connected"
	StateDisconnected = "disconnected"
	StateError        = "error"
)

const (
	defaultInterval = 30 * time.Second
	minBackoff      = 30 * time.Second
	maxBackoff      = 5 * time.Minute
	maxErrorLen     = 300
	// startGrace is how long a freshly started connector may take to reach
	// Cloudflare before it is reported disconnected.
	startGrace   = 60 * time.Second
	probeTimeout = 2 * time.Second
	maxReadyBody = 4096
)

// Composer starts the tunnel Compose project.
type Composer interface {
	Up(ctx context.Context, file, project string, out func(stream, line string)) error
}

// Runtime is the part of runtime.Runtime the manager uses.
type Runtime interface {
	List(ctx context.Context, project string) ([]runtime.Container, error)
	Remove(ctx context.Context, id string, force bool) error
}

// Options configure a [Manager].
type Options struct {
	Config config.Tunnel
	// DataDir is the agent data directory; the tunnel lives in <DataDir>/tunnel.
	DataDir string
	// UID and GID are the identity the connector runs as: the owner of the
	// token file.
	UID, GID int
	// EdgeEnabled and EdgeMode feed the warnings: the tunnel forwards to the
	// edge, which belongs in behind mode.
	EdgeEnabled bool
	EdgeMode    string
	Compose     Composer
	Runtime     Runtime
	Logger      *slog.Logger

	// interval and backoff bounds override the defaults in tests.
	interval, minBackoff, maxBackoff time.Duration
	// readyURL overrides the readiness endpoint in tests.
	readyURL string
}

// Status is the wire form of the tunnel state (TunnelStatus). It never
// contains the token.
type Status struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	// Configured reports whether a token is stored.
	Configured bool `json:"configured"`
	// Ready is whether the connector reports an active connection to
	// Cloudflare; Connections is how many.
	Ready       bool     `json:"ready"`
	Connections int      `json:"connections"`
	Image       string   `json:"image"`
	LastError   string   `json:"last_error,omitempty"`
	Warnings    []string `json:"warnings"`
}

// Manager keeps the tunnel connector in sync with the stored token. It never
// makes the agent fail: problems are logged and reported through
// [Manager.Status], and retried with backoff.
type Manager struct {
	opt      Options
	log      *slog.Logger
	dir      string
	image    string
	readyURL string
	client   *http.Client

	trigger chan struct{}

	// opMu serializes reconcile and Remove.
	opMu sync.Mutex

	mu      sync.Mutex
	lastErr string
	// upAt is when the connector was last (re)started or first found running.
	upAt    time.Time
	started bool
	cancel  context.CancelFunc
	done    chan struct{}

	// Only touched while holding opMu.
	upHash string
}

// New returns a Manager; call [Manager.Start] to run it.
func New(o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.interval <= 0 {
		o.interval = defaultInterval
	}
	if o.minBackoff <= 0 {
		o.minBackoff = minBackoff
	}
	if o.maxBackoff <= 0 {
		o.maxBackoff = maxBackoff
	}
	m := &Manager{
		opt: o, log: o.Logger, image: DefaultImage,
		dir:     filepath.Join(o.DataDir, "tunnel"),
		trigger: make(chan struct{}, 1),
		client:  &http.Client{Timeout: probeTimeout},
	}
	m.readyURL = o.readyURL
	if m.readyURL == "" {
		m.readyURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(o.Config.MetricsPort)) + "/ready"
	}
	return m
}

// Start runs the reconcile loop until [Manager.Close] or ctx is done. With
// the tunnel disabled it does nothing. It returns an error only when called
// twice.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("tunnel: already started")
	}
	m.started = true
	m.mu.Unlock()
	if !m.opt.Config.Enabled {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel, m.done = cancel, make(chan struct{})
	m.mu.Unlock()
	go m.loop(ctx)
	return nil
}

// Trigger asks for a reconcile soon. It never blocks and coalesces bursts.
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Close stops the loop. The connector container keeps running (it survives
// agent restarts).
func (m *Manager) Close() error {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}

// Configure stores the tunnel token and schedules a reconcile. The token is
// validated here and never echoed in an error.
func (m *Manager) Configure(token string) error {
	token, err := CleanToken(token)
	if err != nil {
		return err
	}
	m.opMu.Lock()
	err = storeToken(m.dir, token)
	m.opMu.Unlock()
	if err != nil {
		m.log.Warn("tunnel: store token failed")
		return errors.New("store the tunnel token failed")
	}
	m.mu.Lock()
	m.lastErr = ""
	m.upAt = time.Time{}
	m.mu.Unlock()
	m.log.Info("tunnel: token stored")
	m.Trigger()
	return nil
}

// Remove deletes the stored token and stops and removes the connector.
func (m *Manager) Remove(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if err := removeToken(m.dir); err != nil {
		m.log.Warn("tunnel: remove token failed")
		return errors.New("remove the tunnel token failed")
	}
	m.upHash = ""
	m.mu.Lock()
	m.lastErr = ""
	m.upAt = time.Time{}
	m.mu.Unlock()
	if m.opt.Runtime != nil {
		list, err := m.opt.Runtime.List(ctx, "")
		if err != nil {
			return fmt.Errorf("list containers: %w", err)
		}
		for _, c := range list {
			if c.Labels[LabelComponent] != ComponentTunnel {
				continue
			}
			if err := m.opt.Runtime.Remove(ctx, c.ID, true); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return fmt.Errorf("remove tunnel container: %w", err)
			}
		}
	}
	m.log.Info("tunnel: removed")
	return nil
}

// Status returns a snapshot of the tunnel state, probing the connector's
// readiness endpoint.
func (m *Manager) Status() Status {
	m.mu.Lock()
	lastErr, upAt := m.lastErr, m.upAt
	m.mu.Unlock()
	s := Status{
		Enabled: m.opt.Config.Enabled, Image: m.image,
		Configured: hasToken(m.dir), Warnings: []string{},
	}
	switch {
	case !s.Enabled:
		s.State = StateDisabled
	case !s.Configured:
		s.State = StateUnconfigured
	case lastErr != "":
		s.State, s.LastError = StateError, lastErr
	case upAt.IsZero():
		s.State = StateStarting
	default:
		s.Ready, s.Connections = m.probe()
		switch {
		case s.Ready:
			s.State = StateConnected
		case time.Since(upAt) < startGrace:
			s.State = StateStarting
		default:
			s.State = StateDisconnected
		}
	}
	if s.Enabled {
		switch {
		case !m.opt.EdgeEnabled:
			s.Warnings = append(s.Warnings, "the edge is disabled: the tunnel has nothing to forward to")
		case m.opt.EdgeMode != config.EdgeModeBehind:
			s.Warnings = append(s.Warnings, "edge.mode is not behind: set edge.mode to behind so the edge listens on loopback for the tunnel")
		}
	}
	return s
}

// probe queries the readiness endpoint.
func (m *Manager) probe() (ready bool, connections int) {
	resp, err := m.client.Get(m.readyURL)
	if err != nil {
		return false, 0
	}
	defer resp.Body.Close()
	var body struct {
		ReadyConnections int `json:"readyConnections"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxReadyBody))
	_ = json.Unmarshal(b, &body)
	if body.ReadyConnections < 0 {
		body.ReadyConnections = 0
	}
	return resp.StatusCode == http.StatusOK, body.ReadyConnections
}

func (m *Manager) setErr(msg string) {
	m.mu.Lock()
	m.lastErr = msg
	m.mu.Unlock()
}

func (m *Manager) loop(ctx context.Context) {
	defer close(m.done)
	backoff := time.Duration(0)
	wait := time.Duration(0) // reconcile immediately
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.trigger:
			// A trigger during a backoff retries right away.
		case <-timer.C:
		}
		timer.Stop()
		err := m.reconcile(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if backoff == 0 {
				backoff = m.opt.minBackoff
			} else if backoff *= 2; backoff > m.opt.maxBackoff {
				backoff = m.opt.maxBackoff
			}
			m.log.Warn("tunnel: reconcile failed", "err", err, "retry_in", backoff)
			m.setErr(shorten(err))
			wait = backoff
			continue
		}
		backoff = 0
		m.setErr("")
		wait = m.opt.interval
	}
}

// reconcile regenerates the Compose file and (re)starts the connector when
// needed. Without a token it does nothing.
func (m *Manager) reconcile(ctx context.Context) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if !hasToken(m.dir) {
		m.upHash = ""
		return nil
	}
	if err := ensureDirs(m.dir); err != nil {
		return err
	}
	comp, err := Compose(ComposeOptions{
		Image: m.image, SecretDir: secretDir(m.dir), MetricsPort: m.opt.Config.MetricsPort,
		Generation: generation(m.dir), UID: m.opt.UID, GID: m.opt.GID,
	})
	if err != nil {
		return err
	}
	composeFile := filepath.Join(m.dir, "compose.json")
	if old, err := os.ReadFile(composeFile); err != nil || !bytes.Equal(old, comp) {
		if err := writeAtomic(composeFile, comp, 0o644); err != nil {
			return fmt.Errorf("write compose file: %w", err)
		}
	}
	hash := hashOf(comp)
	running, err := m.running(ctx)
	if err != nil {
		return err
	}
	if hash != m.upHash || !running {
		var tail []string
		err := m.opt.Compose.Up(ctx, composeFile, ProjectName, func(stream, line string) {
			if stream == "stderr" {
				if tail = append(tail, line); len(tail) > 5 {
					tail = tail[1:]
				}
			}
		})
		if err != nil {
			m.log.Warn("tunnel: compose up failed", "err", err)
			if len(tail) > 0 {
				return fmt.Errorf("start tunnel: %s", tail[len(tail)-1])
			}
			return fmt.Errorf("start tunnel: %w", err)
		}
		m.upHash = hash
		m.mu.Lock()
		m.upAt = time.Now()
		m.mu.Unlock()
		m.log.Info("tunnel: started", "image", m.image)
		return nil
	}
	m.mu.Lock()
	if m.upAt.IsZero() {
		m.upAt = time.Now()
	}
	m.mu.Unlock()
	return nil
}

// running reports whether the connector container exists and runs.
func (m *Manager) running(ctx context.Context) (bool, error) {
	list, err := m.opt.Runtime.List(ctx, "")
	if err != nil {
		return false, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range list {
		if c.Labels[LabelComponent] == ComponentTunnel && c.State == "running" {
			return true, nil
		}
	}
	return false, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// shorten turns err into a single short line for the status.
func shorten(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > maxErrorLen {
		s = s[:maxErrorLen-3] + "..."
	}
	return s
}
