package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/client"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/controlplane"
	"github.com/open-southeners/ki-agent/internal/enroll"
	"github.com/open-southeners/ki-agent/internal/enrollment/enrolltest"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/server"
	"github.com/open-southeners/ki-agent/internal/transport/ws"
)

const wsToken = "ws-listener-token"

type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

// env is a real server + manager + enrollment handlers against a fake control plane.
type env struct {
	cp    *enrolltest.ControlPlane
	dir   string
	sock  string
	wsURL string
	mgr   *controlplane.Manager
	h     *Handlers
	logs  *syncBuf
}

// newEnv starts the agent pieces. tweak may adjust the config before use; it
// runs before the handlers apply it.
func newEnv(t *testing.T, tweak func(cp *enrolltest.ControlPlane, cfg *config.Config)) *env {
	t.Helper()
	cp := enrolltest.New(t)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "ws-token")
	if err := os.WriteFile(tokenFile, []byte(wsToken), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Socket.Path = filepath.Join(dir, "run", "ki.sock")
	cfg.WebSocket.Listen = "127.0.0.1:0"
	cfg.WebSocket.TokenFile = tokenFile
	cfg.ControlPlane.ReconnectMin = 10 * time.Millisecond
	cfg.ControlPlane.ReconnectMax = 50 * time.Millisecond
	if tweak != nil {
		tweak(cp, &cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	logs := &syncBuf{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := rpc.NewRouter()
	ag := agent.New()
	ag.Register(router)
	srv, err := server.New(server.Options{Config: &cfg, Router: router, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	mgr := controlplane.NewManager(log)
	h := New(Options{
		Client:  enroll.NewClient(enroll.Options{UserAgent: "ki-agent/test", Sleep: enrolltest.FastSleep}),
		Manager: mgr, Config: cfg.ControlPlane, DataDir: dir,
		Serve:  func(ctx context.Context, c rpc.Conn) { srv.ServeConn(ctx, rpc.TransportControlPlane, c) },
		Logger: log,
	})
	h.Register(router)
	ag.SetControlPlane(mgr.StatusAny)
	if err := h.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = mgr.Run(ctx) }()
	go func() { defer wg.Done(); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("agent did not stop")
		}
	})
	return &env{
		cp: cp, dir: dir, sock: cfg.Socket.Path, h: h, mgr: mgr, logs: logs,
		wsURL: "ws://" + srv.WebSocketAddr() + cfg.WebSocket.Path,
	}
}

func (e *env) creds() string { return filepath.Join(e.dir, CredentialsFile) }

func (e *env) dial(t *testing.T) *client.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, e.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func call(t *testing.T, c *client.Client, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Call(ctx, method, params, result)
}

func wantCode(t *testing.T, err error, code int) *rpc.Error {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want rpc error %d", err, code)
	}
	return re
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (e *env) status() controlplane.Status { return e.mgr.Status() }

// enrollVia runs start and approves, returning the wait result.
func (e *env) enrollVia(t *testing.T, c *client.Client, params map[string]any) string {
	t.Helper()
	var start struct {
		UserCode string `json:"user_code"`
	}
	if err := call(t, c, MethodStart, params, &start); err != nil {
		t.Fatal(err)
	}
	e.cp.Approve()
	var done struct {
		URL string `json:"control_plane_url"`
	}
	if err := call(t, c, MethodWait, nil, &done); err != nil {
		t.Fatal(err)
	}
	return done.URL
}

func TestEnrollmentLifecycle(t *testing.T) {
	e := newEnv(t, nil)
	c := e.dial(t)
	if s := e.status(); s.State != controlplane.StateDisabled || s.Source != "" {
		t.Fatalf("initial status = %+v", s)
	}

	var start map[string]any
	if err := call(t, c, MethodStart, map[string]any{"url": e.cp.URL}, &start); err != nil {
		t.Fatal(err)
	}
	if start["user_code"] != enrolltest.UserCode ||
		start["verification_uri"] != e.cp.URL+"/activate" ||
		start["verification_uri_complete"] != e.cp.URL+"/activate?code="+enrolltest.UserCode ||
		start["expires_at"] == "" {
		t.Fatalf("start result = %v", start)
	}
	if _, err := os.Stat(e.creds()); err == nil {
		t.Fatal("credentials stored before approval")
	}

	e.cp.Approve()
	var done struct {
		URL string `json:"control_plane_url"`
	}
	if err := call(t, c, MethodWait, nil, &done); err != nil {
		t.Fatal(err)
	}
	if done.URL != e.cp.WSURL(enrolltest.AgentPath) {
		t.Fatalf("control_plane_url = %q", done.URL)
	}

	fi, err := os.Stat(e.creds())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file: %v, %v", fi, err)
	}
	saved, err := enroll.LoadCredentials(e.creds())
	if err != nil || saved.Token != enrolltest.Token || saved.URL != done.URL || saved.EnrollmentURL != e.cp.URL || saved.EnrolledAt.IsZero() {
		t.Fatalf("saved = %+v, %v", saved, err)
	}

	// Connected without a restart, reported by agent.info with its source.
	eventually(t, "control plane connection", func() bool { return e.cp.Live(enrolltest.AgentPath) == 1 })
	eventually(t, "connected status", func() bool { return e.status().State == controlplane.StateConnected })
	var info struct {
		ControlPlane struct{ State, Source, URL string } `json:"control_plane"`
	}
	if err := call(t, c, "agent.info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if info.ControlPlane.Source != SourceEnrollment || info.ControlPlane.State != "connected" {
		t.Fatalf("agent.info control_plane = %+v", info.ControlPlane)
	}

	// Enrolled: start needs force; with force it enrolls again.
	wantCode(t, call(t, c, MethodStart, map[string]any{"url": e.cp.URL}, nil), rpc.CodeAlreadyExists)
	if err := call(t, c, MethodStart, map[string]any{"url": e.cp.URL, "force": true}, nil); err != nil {
		t.Fatalf("forced start: %v", err)
	}
	if err := call(t, c, MethodWait, nil, nil); err != nil {
		t.Fatalf("forced wait: %v", err)
	}

	// Remove disconnects and deletes the file; a second remove is NotFound.
	if err := call(t, c, MethodRemove, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.creds()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials still present: %v", err)
	}
	eventually(t, "disconnect", func() bool { return e.cp.Live(enrolltest.AgentPath) == 0 })
	if s := e.status(); s.State != controlplane.StateDisabled {
		t.Fatalf("status after remove = %+v", s)
	}
	re := wantCode(t, call(t, c, MethodRemove, nil, nil), rpc.CodeNotFound)
	if re.Message != "not enrolled" {
		t.Errorf("message = %q", re.Message)
	}

	logs := e.logs.String()
	for _, secret := range []string{enrolltest.Token, enrolltest.DeviceCode} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain a secret %q", secret)
		}
	}
}

func TestStartErrors(t *testing.T) {
	t.Run("no url", func(t *testing.T) {
		e := newEnv(t, nil)
		re := wantCode(t, call(t, e.dial(t), MethodStart, nil, nil), rpc.CodeInvalidParams)
		if !strings.Contains(re.Message, "pass the control plane URL") {
			t.Errorf("message = %q", re.Message)
		}
	})
	t.Run("default url", func(t *testing.T) {
		e := newEnv(t, nil)
		e.h.opts.DefaultURL = e.cp.URL
		if err := call(t, e.dial(t), MethodStart, nil, nil); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("invalid params", func(t *testing.T) {
		e := newEnv(t, nil)
		wantCode(t, call(t, e.dial(t), MethodStart, map[string]any{"url": 5}, nil), rpc.CodeInvalidParams)
	})
	t.Run("bad scheme", func(t *testing.T) {
		e := newEnv(t, nil)
		re := wantCode(t, call(t, e.dial(t), MethodStart, map[string]any{"url": "ftp://x.test"}, nil), rpc.CodeEnrollmentError)
		var d struct{ Reason, Detail string }
		_ = json.Unmarshal(re.Data, &d)
		if d.Reason != enroll.ReasonFailed || d.Detail == "" {
			t.Errorf("data = %s", re.Data)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		e := newEnv(t, nil)
		e.cp.Close()
		re := wantCode(t, call(t, e.dial(t), MethodStart, map[string]any{"url": e.cp.URL}, nil), rpc.CodeEnrollmentError)
		var d struct{ Reason string }
		_ = json.Unmarshal(re.Data, &d)
		if d.Reason != enroll.ReasonUnreachable {
			t.Errorf("data = %s", re.Data)
		}
		// The slot is free again.
		wantCode(t, call(t, e.dial(t), MethodStart, map[string]any{"url": e.cp.URL}, nil), rpc.CodeEnrollmentError)
	})
	t.Run("in progress", func(t *testing.T) {
		e := newEnv(t, nil)
		a, b := e.dial(t), e.dial(t)
		if err := call(t, a, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
			t.Fatal(err)
		}
		wantCode(t, call(t, b, MethodStart, map[string]any{"url": e.cp.URL}, nil), rpc.CodeAlreadyExists)
		wantCode(t, call(t, a, MethodStart, map[string]any{"url": e.cp.URL}, nil), rpc.CodeAlreadyExists)
	})
}

func TestWaitOnlyOnStartingConnection(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.dial(t), e.dial(t)
	re := wantCode(t, call(t, a, MethodWait, nil, nil), rpc.CodeInvalidParams)
	if !strings.Contains(re.Message, "no enrollment in progress on this connection") {
		t.Errorf("message = %q", re.Message)
	}
	if err := call(t, a, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatal(err)
	}
	wantCode(t, call(t, b, MethodWait, nil, nil), rpc.CodeInvalidParams)
}

func TestEnrollmentFailures(t *testing.T) {
	tests := []struct {
		name   string
		fail   func(*enrolltest.ControlPlane)
		reason string
	}{
		{"denied", (*enrolltest.ControlPlane).Deny, enroll.ReasonDenied},
		{"expired", (*enrolltest.ControlPlane).Expire, enroll.ReasonExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, nil)
			c := e.dial(t)
			if err := call(t, c, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
				t.Fatal(err)
			}
			tt.fail(e.cp)
			re := wantCode(t, call(t, c, MethodWait, nil, nil), rpc.CodeEnrollmentError)
			var d struct{ Reason, Detail string }
			_ = json.Unmarshal(re.Data, &d)
			if d.Reason != tt.reason {
				t.Errorf("data = %s", re.Data)
			}
			if _, err := os.Stat(e.creds()); err == nil {
				t.Error("credentials stored after failure")
			}
			if s := e.status(); s.State != controlplane.StateDisabled {
				t.Errorf("status = %+v", s)
			}
			// The failed run does not block a new one.
			if err := call(t, c, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
				t.Fatalf("start after failure: %v", err)
			}
		})
	}
}

// A finished enrollment nobody waited for does not block another connection.
func TestUncollectedResultDoesNotBlock(t *testing.T) {
	e := newEnv(t, nil)
	a, b := e.dial(t), e.dial(t)
	e.cp.Deny()
	if err := call(t, a, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "poll to finish", func() bool {
		e.h.mu.Lock()
		defer e.h.mu.Unlock()
		return e.h.cur != nil && e.h.cur.finished()
	})
	if err := call(t, b, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatalf("start from second connection: %v", err)
	}
}

func TestPeerCloseCancelsPolling(t *testing.T) {
	e := newEnv(t, nil)
	a := e.dial(t)
	if err := call(t, a, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "first poll", func() bool { return e.cp.Polls() > 0 })
	a.Close()

	// Polling stops and the slot is freed.
	var settled int
	eventually(t, "polling to stop", func() bool {
		n := e.cp.Polls()
		time.Sleep(50 * time.Millisecond)
		ok := e.cp.Polls() == n
		settled = n
		return ok
	})
	e.cp.Approve()
	time.Sleep(100 * time.Millisecond)
	if n := e.cp.Polls(); n != settled {
		t.Errorf("polled %d times after the connection closed", n-settled)
	}
	if _, err := os.Stat(e.creds()); err == nil {
		t.Error("credentials stored for a cancelled enrollment")
	}
	b := e.dial(t)
	if err := call(t, b, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatalf("start after peer close: %v", err)
	}
}

func TestRemoveCancelsEnrollmentInProgress(t *testing.T) {
	e := newEnv(t, nil)
	c := e.dial(t)
	if err := call(t, c, MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatal(err)
	}
	wantCode(t, call(t, c, MethodRemove, nil, nil), rpc.CodeNotFound)
	wantCode(t, call(t, c, MethodWait, nil, nil), rpc.CodeInvalidParams)
	e.cp.Approve()
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(e.creds()); err == nil {
		t.Error("credentials stored after remove cancelled the enrollment")
	}
}

func TestRemoveFallsBackToConfig(t *testing.T) {
	e := newEnv(t, func(cp *enrolltest.ControlPlane, cfg *config.Config) {
		cp.AddEndpoint(t, "/cfg", "cfg-token")
		tok := filepath.Join(cfg.DataDir, "cfg-token")
		if err := os.WriteFile(tok, []byte("cfg-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.ControlPlane.URL = cp.WSURL("/cfg")
		cfg.ControlPlane.TokenFile = tok
	})
	if s := e.status(); s.Source != SourceConfig {
		t.Fatalf("startup source = %q, want config", s.Source)
	}
	eventually(t, "config connection", func() bool { return e.cp.Live("/cfg") == 1 })

	c := e.dial(t)
	e.enrollVia(t, c, map[string]any{"url": e.cp.URL})
	eventually(t, "enrolled connection", func() bool { return e.cp.Live(enrolltest.AgentPath) == 1 })
	eventually(t, "config connection closed", func() bool { return e.cp.Live("/cfg") == 0 })
	if s := e.status(); s.Source != SourceEnrollment {
		t.Fatalf("source = %q, want enrollment", s.Source)
	}

	if err := call(t, c, MethodRemove, nil, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "fallback connection", func() bool { return e.cp.Live("/cfg") == 1 && e.cp.Live(enrolltest.AgentPath) == 0 })
	if s := e.status(); s.Source != SourceConfig {
		t.Fatalf("source after remove = %q, want config", s.Source)
	}
}

func TestApplyAtStartup(t *testing.T) {
	writeCreds := func(t *testing.T, dir string, mode os.FileMode, cp *enrolltest.ControlPlane) {
		t.Helper()
		if err := enroll.SaveCredentials(filepath.Join(dir, CredentialsFile), enroll.Credentials{
			URL: cp.WSURL(enrolltest.AgentPath), Token: enrolltest.Token, EnrolledAt: time.Now(), EnrollmentURL: cp.URL,
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, CredentialsFile), mode); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("credentials without config", func(t *testing.T) {
		cp := enrolltest.New(t)
		dir := t.TempDir()
		writeCreds(t, dir, 0o600, cp)
		h := New(Options{Manager: controlplane.NewManager(nil), Config: config.Default().ControlPlane, DataDir: dir,
			Serve: func(context.Context, rpc.Conn) {}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err := h.Apply(); err != nil {
			t.Fatal(err)
		}
		if s := h.opts.Manager.Status(); s.Source != SourceEnrollment || s.URL != cp.WSURL(enrolltest.AgentPath) {
			t.Errorf("status = %+v", s)
		}
	})
	t.Run("credentials override config and bypass token_file", func(t *testing.T) {
		dir := t.TempDir()
		cp := enrolltest.New(t)
		writeCreds(t, dir, 0o600, cp)
		cfgCP := config.Default().ControlPlane
		cfgCP.URL = "wss://config.example.test/agent" // no token_file
		logs := &syncBuf{}
		h := New(Options{Manager: controlplane.NewManager(nil), Config: cfgCP, DataDir: dir,
			Serve: func(context.Context, rpc.Conn) {}, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
		if err := h.Apply(); err != nil {
			t.Fatal(err)
		}
		if s := h.opts.Manager.Status(); s.Source != SourceEnrollment || strings.Contains(s.URL, "config.example") {
			t.Errorf("status = %+v", s)
		}
		if !strings.Contains(logs.String(), "override") || strings.Contains(logs.String(), enrolltest.Token) {
			t.Errorf("logs = %s", logs.String())
		}
	})
	t.Run("insecure credentials file is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeCreds(t, dir, 0o644, enrolltest.New(t))
		h := New(Options{Manager: controlplane.NewManager(nil), Config: config.Default().ControlPlane, DataDir: dir})
		if err := h.Apply(); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("nothing configured", func(t *testing.T) {
		h := New(Options{Manager: controlplane.NewManager(nil), Config: config.Default().ControlPlane, DataDir: t.TempDir()})
		if err := h.Apply(); err != nil {
			t.Fatal(err)
		}
		if s := h.opts.Manager.Status(); s.State != controlplane.StateDisabled {
			t.Errorf("status = %+v", s)
		}
	})
}

func TestLocalOnlyOverWebSocket(t *testing.T) {
	e := newEnv(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := ws.Dial(ctx, e.wsURL, wsToken)
	if err != nil {
		t.Fatal(err)
	}
	p := rpc.NewPeer(conn, rpc.NewRouter())
	go p.Run(context.Background())
	t.Cleanup(func() { p.Close() })
	if err := p.Call(ctx, "agent.hello", map[string]any{"protocol": "1.0.0"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{MethodStart, MethodWait, MethodRemove} {
		re := wantCode(t, p.Call(ctx, m, map[string]any{"url": e.cp.URL}, nil), rpc.CodeUnauthorized)
		if re.Message != "only available on the local socket" {
			t.Errorf("%s message = %q", m, re.Message)
		}
	}
	if e.cp.Devices() != 0 {
		t.Error("a refused call reached the enrollment server")
	}
	// The unix socket is allowed.
	if err := call(t, e.dial(t), MethodStart, map[string]any{"url": e.cp.URL}, nil); err != nil {
		t.Fatal(err)
	}
}
