// Package controlplane maintains the agent's outbound WebSocket connection to
// a control plane. The control plane acts as the JSON-RPC client of the normal
// agent API over that connection; the Connector only dials, hands the
// connection to a serve function and reconnects with jittered exponential
// backoff when it ends.
package controlplane

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/ws"
	"github.com/open-southeners/ki-agent/internal/version"
)

// Connection states reported in Status.State.
const (
	StateDisabled   = "disabled"
	StateConnecting = "connecting"
	StateConnected  = "connected"
	StateBackoff    = "backoff"
)

// Defaults applied for zero Options fields.
const (
	DefaultReconnectMin = time.Second
	DefaultReconnectMax = time.Minute
	// DialTimeout bounds the WebSocket handshake of one attempt.
	DialTimeout = 15 * time.Second
)

// Status is a snapshot of the connection, reported by agent.info.
type Status struct {
	State string `json:"state"`
	// URL is the control plane endpoint without userinfo, query or fragment.
	URL string `json:"url,omitempty"`
	// ConnectedSince is set while State is "connected".
	ConnectedSince *time.Time `json:"connected_since,omitempty"`
	// LastError describes the most recent failed or ended connection.
	LastError string `json:"last_error,omitempty"`
	// Reconnects counts connection attempts made after the first one.
	Reconnects int `json:"reconnects"`
	// NextAttempt is set while State is "backoff".
	NextAttempt *time.Time `json:"next_attempt,omitempty"`
}

// Options configures a Connector.
type Options struct {
	// URL is the ws:// or wss:// endpoint; Token the bearer token. The token
	// is never logged or included in Status.
	URL   string
	Token string
	// TLSConfig is used for wss:// URLs (nil means the defaults).
	TLSConfig *tls.Config

	// Heartbeat settings for the client-side ping loop (defaults 15s / 45s).
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	// ReconnectMin and ReconnectMax bound the backoff (defaults 1s / 60s).
	ReconnectMin time.Duration
	ReconnectMax time.Duration

	// Serve runs the JSON-RPC peer over a connected conn and returns when it
	// ends. Its ctx carries agent.WithHelloHook, which Serve must pass on to
	// the peer (server.ServeConn does); the first successful agent.hello
	// resets the backoff.
	Serve  func(ctx context.Context, c rpc.Conn)
	Logger *slog.Logger

	// Test hooks. Now defaults to time.Now; Rand returns [0,1) and defaults
	// to math/rand/v2.
	Now  func() time.Time
	Rand func() float64
}

// Connector keeps the outbound connection alive until its context ends.
type Connector struct {
	opts Options
	log  *slog.Logger
	url  string // sanitized

	mu     sync.Mutex
	status Status
}

// New validates opts and returns a Connector. Run starts it.
func New(opts Options) (*Connector, error) {
	if opts.URL == "" || opts.Token == "" || opts.Serve == nil {
		return nil, errors.New("controlplane: URL, Token and Serve are required")
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = ws.DefaultHeartbeatInterval
	}
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = ws.DefaultHeartbeatTimeout
	}
	if opts.ReconnectMin <= 0 {
		opts.ReconnectMin = DefaultReconnectMin
	}
	if opts.ReconnectMax <= 0 {
		opts.ReconnectMax = DefaultReconnectMax
	}
	if opts.ReconnectMax < opts.ReconnectMin {
		opts.ReconnectMax = opts.ReconnectMin
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Rand == nil {
		opts.Rand = rand.Float64
	}
	c := &Connector{opts: opts, log: opts.Logger, url: SanitizeURL(opts.URL)}
	if c.log == nil {
		c.log = slog.Default()
	}
	c.status = Status{State: StateConnecting, URL: c.url}
	return c, nil
}

// SanitizeURL strips userinfo, query and fragment from raw. It returns "" when
// raw cannot be parsed, so nothing unparsed is ever echoed.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// Status returns a snapshot of the connection state. It implements the
// function agent.Agent.SetControlPlane expects when wrapped (see StatusAny).
func (c *Connector) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.status
	if s.ConnectedSince != nil {
		t := *s.ConnectedSince
		s.ConnectedSince = &t
	}
	if s.NextAttempt != nil {
		t := *s.NextAttempt
		s.NextAttempt = &t
	}
	return s
}

// StatusAny is Status as an any, for agent.Agent.SetControlPlane.
func (c *Connector) StatusAny() any { return c.Status() }

func (c *Connector) update(fn func(*Status)) {
	c.mu.Lock()
	fn(&c.status)
	c.mu.Unlock()
}

// Run dials, serves and reconnects until ctx is cancelled, then returns nil.
// Cancelling ctx interrupts an in-progress dial or backoff wait immediately
// and ends a live connection.
func (c *Connector) Run(ctx context.Context) error {
	failures := 0 // consecutive attempts that did not reach agent.hello
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if attempt > 0 {
			c.update(func(s *Status) { s.Reconnects++ })
		}
		c.update(func(s *Status) { s.State = StateConnecting; s.NextAttempt = nil })

		delay, helloed, err := c.once(ctx)
		if ctx.Err() != nil {
			break
		}
		if helloed {
			failures = 0
		}
		if delay == 0 {
			delay = c.backoff(failures)
			failures++
		}
		msg := c.redact(err)
		c.log.Warn("control plane connection ended", "url", c.url, "err", msg, "retry_in", delay.Round(time.Millisecond))
		next := c.opts.Now().Add(delay)
		c.update(func(s *Status) {
			s.State = StateBackoff
			s.ConnectedSince = nil
			s.NextAttempt = &next
			s.LastError = msg
		})
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
	}
	c.update(func(s *Status) {
		s.State = StateBackoff
		s.ConnectedSince = nil
		s.NextAttempt = nil
	})
	return nil
}

// once makes one dial+serve cycle. It returns a forced retry delay (non-zero
// only for 401/403), whether agent.hello succeeded, and why the cycle ended.
func (c *Connector) once(ctx context.Context) (forced time.Duration, helloed bool, err error) {
	h := http.Header{}
	h.Set("User-Agent", "ki-agent/"+version.Version)
	h.Set("Ki-Protocol", api.ProtocolVersion)
	dctx, cancel := context.WithTimeout(ctx, DialTimeout)
	conn, resp, derr := ws.DialWith(dctx, c.opts.URL, ws.DialOptions{
		Token:             c.opts.Token,
		Header:            h,
		TLSConfig:         c.opts.TLSConfig,
		HeartbeatInterval: c.opts.HeartbeatInterval,
		HeartbeatTimeout:  c.opts.HeartbeatTimeout,
	})
	cancel()
	if derr != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			err := fmt.Errorf("control plane rejected credentials: HTTP %d", resp.StatusCode)
			c.log.Error("control plane rejected credentials; retrying at the maximum interval",
				"url", c.url, "status", resp.StatusCode)
			return c.opts.ReconnectMax, false, err
		}
		return 0, false, fmt.Errorf("dial: %w", derr)
	}

	start := c.opts.Now()
	c.update(func(s *Status) { s.State = StateConnected; s.ConnectedSince = &start; s.NextAttempt = nil })
	c.log.Info("control plane connected", "url", c.url)
	var hm sync.Mutex
	hook := func() { hm.Lock(); helloed = true; hm.Unlock() }
	c.opts.Serve(agent.WithHelloHook(ctx, hook), conn)
	hm.Lock()
	defer hm.Unlock()
	return 0, helloed, errors.New("connection closed")
}

// backoff returns rand(0, min(max, min*2^failures)) (full jitter).
func (c *Connector) backoff(failures int) time.Duration {
	ceil := c.opts.ReconnectMax
	if failures < 32 {
		if d := c.opts.ReconnectMin << failures; d > 0 && d < ceil {
			ceil = d
		}
	}
	return time.Duration(c.opts.Rand() * float64(ceil))
}

// redact removes the URL (which may carry credentials in userinfo or query)
// from err before it is exposed through Status.
func (c *Connector) redact(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, ue.URL, c.url)
	}
	msg = strings.ReplaceAll(msg, c.opts.URL, c.url)
	if c.opts.Token != "" {
		msg = strings.ReplaceAll(msg, c.opts.Token, "[redacted]")
	}
	return msg
}
