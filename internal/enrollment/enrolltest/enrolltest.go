// Package enrolltest provides a fake control plane for tests: an HTTP device
// flow enrollment server plus WebSocket endpoints that accept given bearer
// tokens and count their live connections.
package enrolltest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/ws"
)

// Fixed secrets handed out by the fake, so tests can assert they never leak.
const (
	DeviceCode = "device-code-SECRET"
	Token      = "issued-token-SECRET"
	UserCode   = "ABCD-EFGH"
	// AgentPath is the WebSocket path that accepts Token and is announced as
	// control_plane_url.
	AgentPath = "/agent"
)

// ControlPlane is a running fake. All methods are safe for concurrent use.
type ControlPlane struct {
	// URL is the base URL for enrollment (plain http on 127.0.0.1).
	URL string

	srv *httptest.Server
	mux *http.ServeMux

	state   atomic.Int32 // 0 pending, 1 approved, 2 denied, 3 expired
	polls   atomic.Int32
	devices atomic.Int32

	mu   sync.Mutex
	live map[string]int
}

const (
	statePending = iota
	stateApproved
	stateDenied
	stateExpired
)

// New starts a fake control plane that stays pending until Approve, Deny or
// Expire is called. It accepts Token on AgentPath.
func New(t testing.TB) *ControlPlane {
	t.Helper()
	c := &ControlPlane{live: map[string]int{}, mux: http.NewServeMux()}
	c.mux.HandleFunc("/.well-known/ki-agent-enrollment", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, fmt.Sprintf(`{"device_authorization_endpoint":"%s/device","token_endpoint":"%s/token"}`, c.URL, c.URL))
	})
	c.mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		c.devices.Add(1)
		_ = r.ParseForm()
		writeJSON(w, 200, `{"device_code":"`+DeviceCode+`","user_code":"`+UserCode+`","verification_uri":"`+c.URL+
			`/activate","verification_uri_complete":"`+c.URL+`/activate?code=`+UserCode+`","expires_in":600,"interval":1}`)
	})
	c.mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		c.polls.Add(1)
		switch c.state.Load() {
		case stateApproved:
			writeJSON(w, 200, `{"access_token":"`+Token+`","token_type":"Bearer","control_plane_url":"`+c.WSURL(AgentPath)+`"}`)
		case stateDenied:
			writeJSON(w, 400, `{"error":"access_denied"}`)
		case stateExpired:
			writeJSON(w, 400, `{"error":"expired_token"}`)
		default:
			writeJSON(w, 400, `{"error":"authorization_pending"}`)
		}
	})
	c.srv = httptest.NewServer(c.mux)
	c.URL = c.srv.URL
	t.Cleanup(c.srv.Close)
	c.AddEndpoint(t, AgentPath, Token)
	return c
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// WSURL returns the ws:// URL of path on the fake.
func (c *ControlPlane) WSURL(path string) string {
	return "ws" + strings.TrimPrefix(c.srv.URL, "http") + path
}

// AddEndpoint serves a WebSocket endpoint at path accepting token. Each
// connection is counted in Live until it ends; the fake performs agent.hello
// as a control plane would.
func (c *ControlPlane) AddEndpoint(t testing.TB, path, token string) {
	t.Helper()
	h, err := ws.NewHandler(ws.Options{Token: token}, func(ctx context.Context, conn rpc.Conn) {
		c.mu.Lock()
		c.live[path]++
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.live[path]--
			c.mu.Unlock()
		}()
		p := rpc.NewPeer(conn, rpc.NewRouter())
		go func() { _ = p.Run(ctx) }()
		defer p.Close()
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = p.Call(cctx, "agent.hello", map[string]any{"protocol": "1.0.0"}, nil)
		cancel()
		select {
		case <-p.Done():
		case <-ctx.Done():
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	c.mux.Handle(path, h)
}

// Live returns the number of open WebSocket connections on path.
func (c *ControlPlane) Live(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live[path]
}

// Approve makes the next token poll succeed.
func (c *ControlPlane) Approve() { c.state.Store(stateApproved) }

// Deny makes the next token poll answer access_denied.
func (c *ControlPlane) Deny() { c.state.Store(stateDenied) }

// Expire makes the next token poll answer expired_token.
func (c *ControlPlane) Expire() { c.state.Store(stateExpired) }

// Polls returns how many times the token endpoint was called.
func (c *ControlPlane) Polls() int { return int(c.polls.Load()) }

// Devices returns how many device authorizations were requested.
func (c *ControlPlane) Devices() int { return int(c.devices.Load()) }

// Close shuts the HTTP server down, so later requests fail to connect.
func (c *ControlPlane) Close() { c.srv.Close() }

// FastSleep is an enroll.Options.Sleep that waits a few milliseconds instead
// of the polling interval.
func FastSleep(ctx context.Context, _ time.Duration) error {
	t := time.NewTimer(5 * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
