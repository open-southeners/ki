package controlplane

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki/api"
	"github.com/open-southeners/ki/internal/agent"
	"github.com/open-southeners/ki/internal/config"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/server"
	"github.com/open-southeners/ki/internal/transport/ws"
)

const testToken = "cp-secret-token"

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// agentSide is a real server (router + handshake gate) the connector serves on.
type agentSide struct {
	srv  *server.Server
	ag   *agent.Agent
	logs *syncBuf
	done chan error
	stop context.CancelFunc
}

func newAgentSide(t *testing.T) *agentSide {
	t.Helper()
	dir, err := os.MkdirTemp("", "kicp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Socket.Path = filepath.Join(dir, "ki.sock")
	cfg.API.Listen = "127.0.0.1:0"
	cfg.API.EdgeListen = "127.0.0.1:0"
	router := rpc.NewRouter()
	ag := agent.New()
	ag.Register(router)
	logs := &syncBuf{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	srv, err := server.New(server.Options{Config: &cfg, Router: router, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &agentSide{srv: srv, ag: ag, logs: logs, done: make(chan error, 1), stop: cancel}
	go func() { a.done <- srv.Serve(ctx) }()
	t.Cleanup(cancel)
	return a
}

// fakeCP is a control plane: it authenticates agents and hands each accepted
// connection to the test as a client-side rpc.Peer.
type fakeCP struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
	reject  int // HTTP status to answer instead of upgrading; 0 = accept
	peers   chan *rpc.Peer
	updates chan rpc.Update
}

func newFakeCP(t *testing.T, tlsServer bool) *fakeCP {
	t.Helper()
	f := &fakeCP{peers: make(chan *rpc.Peer, 16), updates: make(chan rpc.Update, 64)}
	inner, err := ws.NewHandler(ws.Options{Authenticate: ws.BearerAuth(testToken)}, func(ctx context.Context, c rpc.Conn, _ any) {
		r := rpc.NewRouter()
		r.Handle(agent.NotifyHeartbeat, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
			var u rpc.Update
			if err := json.Unmarshal(params, &u); err != nil {
				return nil, err
			}
			f.updates <- u
			return nil, nil
		})
		p := rpc.NewPeer(c, r)
		f.peers <- p
		_ = p.Run(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = append(f.headers, r.Header.Clone())
		reject := f.reject
		f.mu.Unlock()
		if reject != 0 {
			http.Error(w, "no", reject)
			return
		}
		inner.ServeHTTP(w, r)
	})
	if tlsServer {
		f.Server = httptest.NewTLSServer(h)
	} else {
		f.Server = httptest.NewServer(h)
	}
	t.Cleanup(f.Close)
	return f
}

func (f *fakeCP) setReject(code int) {
	f.mu.Lock()
	f.reject = code
	f.mu.Unlock()
}

func (f *fakeCP) wsURL() string {
	return "ws" + strings.TrimPrefix(f.URL, "http") + "/agent"
}

func (f *fakeCP) peer(t *testing.T) *rpc.Peer {
	t.Helper()
	select {
	case p := <-f.peers:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not connect")
		return nil
	}
}

func call(t *testing.T, p *rpc.Peer, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Call(ctx, method, params, result)
}

func hello(t *testing.T, p *rpc.Peer) {
	t.Helper()
	if err := call(t, p, "agent.hello", map[string]any{"protocol": api.ProtocolVersion}, nil); err != nil {
		t.Fatalf("hello: %v", err)
	}
}

// startConnector runs a connector serving on a, with fast timings.
func startConnector(t *testing.T, a *agentSide, mod func(*Options)) (*Connector, func()) {
	t.Helper()
	opts := Options{
		Token:             testToken,
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  time.Second,
		ReconnectMin:      5 * time.Millisecond,
		ReconnectMax:      20 * time.Millisecond,
		Serve:             func(ctx context.Context, c rpc.Conn) { a.srv.ServeConn(ctx, "control-plane", c) },
		Logger:            slog.New(slog.NewJSONHandler(a.logs, nil)),
	}
	mod(&opts)
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	a.ag.SetControlPlane(c.StatusAny)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = c.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("connector did not stop")
		}
	}
	t.Cleanup(stop)
	return c, stop
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestConnectHelloInfoHeartbeat(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	c, _ := startConnector(t, a, func(o *Options) { o.URL = f.wsURL() + "?key=querysecret" })
	p := f.peer(t)

	f.mu.Lock()
	h := f.headers[0]
	f.mu.Unlock()
	if h.Get("Authorization") != "Bearer "+testToken || !strings.HasPrefix(h.Get("User-Agent"), "ki-agent/") ||
		h.Get("Ki-Protocol") != api.ProtocolVersion {
		t.Fatalf("unexpected request headers: %v", h)
	}

	// The gate applies on this connection too.
	var re *rpc.Error
	if err := call(t, p, "agent.info", nil, nil); !errors.As(err, &re) || re.Code != rpc.CodeHandshakeRequired {
		t.Fatalf("info before hello: %v", err)
	}
	hello(t, p)

	var info struct {
		ControlPlane Status `json:"control_plane"`
	}
	if err := call(t, p, "agent.info", nil, &info); err != nil {
		t.Fatal(err)
	}
	st := info.ControlPlane
	if st.State != StateConnected || st.ConnectedSince == nil || st.URL != f.wsURL() || strings.Contains(st.URL, "querysecret") {
		t.Fatalf("status = %+v", st)
	}
	if got := c.Status(); got.State != StateConnected {
		t.Fatalf("Status() = %+v", got)
	}

	if err := call(t, p, "agent.heartbeat.subscribe", map[string]any{"interval_ms": 250}, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.updates:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat update")
	}
	if s := a.logs.String(); strings.Contains(s, testToken) || strings.Contains(s, "querysecret") {
		t.Fatalf("logs leak credentials:\n%s", s)
	}
}

func TestReconnectAfterDropResetsBackoff(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	var calls atomic.Int32
	c, _ := startConnector(t, a, func(o *Options) {
		o.URL = f.wsURL()
		o.ReconnectMin = 150 * time.Millisecond
		o.ReconnectMax = 10 * time.Second
		o.Rand = func() float64 { calls.Add(1); return 1 } // always the ceiling
	})

	// Connections without hello, dropped by the control plane: the delay
	// grows 150ms, 300ms (rand pinned to the ceiling).
	f.peer(t).Close()
	f.peer(t).Close()
	// The third says hello before dropping: the next delay is back at 150ms
	// rather than the 600ms it would be without the reset.
	p := f.peer(t)
	hello(t, p)
	p.Close()
	start := time.Now()
	p = f.peer(t)
	if d := time.Since(start); d > 450*time.Millisecond {
		t.Fatalf("reconnect took %v; backoff did not reset", d)
	}
	waitFor(t, "reconnect counter", func() bool { return c.Status().Reconnects >= 3 })
	if calls.Load() < 3 {
		t.Fatalf("expected jitter draws, got %d", calls.Load())
	}
	hello(t, p)
}

func TestBackoffGrowthAndJitter(t *testing.T) {
	c, err := New(Options{
		URL: "ws://127.0.0.1:1/x", Token: "t", Serve: func(context.Context, rpc.Conn) {},
		ReconnectMin: time.Second, ReconnectMax: 10 * time.Second,
		Rand: func() float64 { return 0.5 },
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for n, w := range want {
		if got := c.backoff(n); got != w {
			t.Errorf("backoff(%d) = %v, want %v", n, got, w)
		}
	}
	if got := c.backoff(1000); got != 5*time.Second {
		t.Errorf("backoff(1000) = %v", got)
	}
	c.opts.Rand = func() float64 { return 0 }
	if got := c.backoff(3); got != 0 {
		t.Errorf("zero jitter = %v", got)
	}
}

func TestUnauthorizedBacksOffAtMax(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	f.setReject(http.StatusUnauthorized)
	now := time.Now()
	c, _ := startConnector(t, a, func(o *Options) {
		o.URL = f.wsURL()
		o.ReconnectMin = time.Millisecond
		o.ReconnectMax = 10 * time.Minute
		o.Now = func() time.Time { return now }
	})
	waitFor(t, "backoff status", func() bool { return c.Status().State == StateBackoff })
	st := c.Status()
	if !strings.Contains(st.LastError, "401") || st.NextAttempt == nil || st.NextAttempt.Sub(now) != 10*time.Minute {
		t.Fatalf("status = %+v", st)
	}
	if !strings.Contains(a.logs.String(), `"level":"ERROR"`) {
		t.Fatalf("expected an error-level log:\n%s", a.logs.String())
	}
}

func TestDialFailureRedactsURL(t *testing.T) {
	a := newAgentSide(t)
	c, _ := startConnector(t, a, func(o *Options) {
		o.URL = "ws://user:pw@127.0.0.1:1/agent?token=querysecret" // nothing listens on port 1
		o.ReconnectMax = 5 * time.Minute
	})
	waitFor(t, "backoff status", func() bool { return c.Status().LastError != "" })
	st := c.Status()
	if st.URL != "ws://127.0.0.1:1/agent" {
		t.Fatalf("URL = %q", st.URL)
	}
	for _, s := range []string{st.LastError, a.logs.String()} {
		if strings.Contains(s, "querysecret") || strings.Contains(s, "pw@") || strings.Contains(s, testToken) {
			t.Fatalf("credentials leaked: %s", s)
		}
	}
}

func TestSanitizeURL(t *testing.T) {
	tests := map[string]string{
		"wss://u:p@cp.example.com:8443/a/b?x=1#frag": "wss://cp.example.com:8443/a/b",
		"ws://127.0.0.1:1/x?":                        "ws://127.0.0.1:1/x",
		"wss://cp.example.com":                       "wss://cp.example.com",
		"://bad":                                     "",
	}
	for in, want := range tests {
		if got := SanitizeURL(in); got != want {
			t.Errorf("SanitizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWSSWithCAFile(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, true)

	// Without trusting the test CA the handshake fails.
	cfg := config.ControlPlane{}
	tlsDefault, err := cfg.TLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	c, stop := startConnector(t, a, func(o *Options) { o.URL = f.wsURL(); o.TLSConfig = tlsDefault })
	waitFor(t, "tls failure", func() bool { return strings.Contains(c.Status().LastError, "certificate") })
	stop()

	// With the server certificate in ca_file it connects.
	pemPath := filepath.Join(t.TempDir(), "ca.pem")
	cert := f.Certificate()
	if err := os.WriteFile(pemPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.CAFile = pemPath
	tlsCA, err := cfg.TLSConfig()
	if err != nil || tlsCA.MinVersion != tls.VersionTLS12 {
		t.Fatalf("TLSConfig: %+v, %v", tlsCA, err)
	}
	startConnector(t, a, func(o *Options) { o.URL = f.wsURL(); o.TLSConfig = tlsCA })
	hello(t, f.peer(t))
}

func TestGracefulShutdownWithOpenConnection(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	_, stop := startConnector(t, a, func(o *Options) { o.URL = f.wsURL() })
	p := f.peer(t)
	hello(t, p)
	if err := call(t, p, "agent.heartbeat.subscribe", nil, nil); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	a.stop() // server shutdown closes the control-plane peer
	select {
	case err := <-a.done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop with a control-plane connection open")
	}
	stop()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("control plane side still open")
	}
}

func TestShutdownInterruptsBackoffAndDial(t *testing.T) {
	// A server that accepts TCP but never answers the handshake.
	hang := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-hang }))
	defer srv.Close()
	defer close(hang)

	a := newAgentSide(t)
	_, stop := startConnector(t, a, func(o *Options) { o.URL = "ws" + strings.TrimPrefix(srv.URL, "http") })
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	stop()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancelling mid-dial took %v", d)
	}

	// And mid-backoff.
	c, stop2 := startConnector(t, a, func(o *Options) {
		o.URL = "ws://127.0.0.1:1/x"
		o.ReconnectMin = time.Hour
		o.ReconnectMax = time.Hour
		o.Rand = func() float64 { return 1 }
	})
	waitFor(t, "backoff", func() bool { return c.Status().State == StateBackoff })
	start = time.Now()
	stop2()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("cancelling mid-backoff took %v", d)
	}
}

func TestNewRequiresFields(t *testing.T) {
	for _, o := range []Options{{}, {URL: "ws://x", Token: "t"}, {URL: "ws://x", Serve: func(context.Context, rpc.Conn) {}}} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) succeeded", o)
		}
	}
}

// managerOpts returns Options for a manager-run connector against f.
func managerOpts(a *agentSide, f *fakeCP) *Options {
	return &Options{
		URL:               f.wsURL(),
		Token:             testToken,
		HeartbeatInterval: 100 * time.Millisecond,
		HeartbeatTimeout:  time.Second,
		ReconnectMin:      5 * time.Millisecond,
		ReconnectMax:      20 * time.Millisecond,
		Serve:             func(ctx context.Context, c rpc.Conn) { a.srv.ServeConn(ctx, "control-plane", c) },
	}
}

func runManager(t *testing.T, m *Manager) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.Run(ctx); close(done) }()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("manager did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func TestManagerDisabledByDefault(t *testing.T) {
	m := NewManager(nil)
	if s := m.Status(); s.State != StateDisabled || s.Source != "" {
		t.Fatalf("status = %+v", s)
	}
	runManager(t, m)
	if err := m.Apply(nil, "config"); err != nil {
		t.Fatal(err)
	}
	if s := m.StatusAny().(Status); s.State != StateDisabled {
		t.Fatalf("status = %+v", s)
	}
}

func TestManagerApplyBeforeRun(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	m := NewManager(nil)
	if err := m.Apply(managerOpts(a, f), "config"); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.State != StateConnecting || s.Source != "config" {
		t.Fatalf("status = %+v", s)
	}
	select {
	case <-f.peers:
		t.Fatal("connected before Run")
	case <-time.After(100 * time.Millisecond):
	}
	runManager(t, m)
	hello(t, f.peer(t))
	waitFor(t, "connected", func() bool { return m.Status().State == StateConnected })
	if s := m.Status(); s.Source != "config" || s.URL != f.wsURL() {
		t.Fatalf("status = %+v", s)
	}
}

func TestManagerApplyAfterRunSwapAndDisable(t *testing.T) {
	a := newAgentSide(t)
	fa := newFakeCP(t, false)
	fb := newFakeCP(t, false)
	m := NewManager(nil)
	runManager(t, m)

	if err := m.Apply(managerOpts(a, fa), "config"); err != nil {
		t.Fatal(err)
	}
	pa := fa.peer(t)
	hello(t, pa)
	waitFor(t, "connected", func() bool { return m.Status().State == StateConnected })

	// An invalid configuration leaves the running connection intact.
	if err := m.Apply(&Options{URL: fb.wsURL()}, "enrollment"); err == nil {
		t.Fatal("invalid Apply succeeded")
	}
	if s := m.Status(); s.State != StateConnected || s.Source != "config" {
		t.Fatalf("status after bad apply = %+v", s)
	}
	select {
	case <-pa.Done():
		t.Fatal("bad apply dropped the connection")
	default:
	}

	// Swap A -> B: A's connection is closed, B is dialed.
	if err := m.Apply(managerOpts(a, fb), "enrollment"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pa.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("old connection still open after swap")
	}
	pb := fb.peer(t)
	hello(t, pb)
	waitFor(t, "connected to B", func() bool {
		s := m.Status()
		return s.State == StateConnected && s.Source == "enrollment" && s.URL == fb.wsURL()
	})
	select {
	case <-fa.peers:
		t.Fatal("old connector reconnected after swap")
	case <-time.After(100 * time.Millisecond):
	}

	// Disable drops B and stays down.
	if err := m.Apply(nil, "enrollment"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pb.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("connection still open after disable")
	}
	if s := m.Status(); s.State != StateDisabled {
		t.Fatalf("status = %+v", s)
	}
	select {
	case <-fb.peers:
		t.Fatal("reconnected after disable")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestManagerCtxEndStopsEverything(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	m := NewManager(nil)
	stop := runManager(t, m)
	if err := m.Apply(managerOpts(a, f), "config"); err != nil {
		t.Fatal(err)
	}
	p := f.peer(t)
	hello(t, p)
	stop()
	select {
	case <-p.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("connection still open after Run returned")
	}
	if err := m.Apply(managerOpts(a, f), "config"); !errors.Is(err, ErrManagerStopped) {
		t.Fatalf("Apply after Run = %v", err)
	}
	if s := m.Status(); s.State != StateDisabled {
		t.Fatalf("status = %+v", s)
	}
	if err := m.Run(context.Background()); err == nil {
		t.Fatal("second Run succeeded")
	}
}

func TestManagerConcurrentApplyAndStatus(t *testing.T) {
	a := newAgentSide(t)
	f := newFakeCP(t, false)
	m := NewManager(nil)
	stop := runManager(t, m)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				var o *Options
				if j%3 != 2 {
					o = managerOpts(a, f)
				}
				if err := m.Apply(o, "config"); err != nil {
					t.Errorf("Apply: %v", err)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = m.Status()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	stop()
}
