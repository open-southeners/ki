package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/unixsock"
	"github.com/open-southeners/ki-agent/internal/transport/ws"
)

const testToken = "s3cret-token"

type harness struct {
	sock   string
	wsURL  string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	logs   *syncBuf
}

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

func start(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Socket.Path = filepath.Join(dir, "run", "ki.sock") // parent must be created
	cfg.WebSocket.Listen = "127.0.0.1:0"
	cfg.WebSocket.TokenFile = tokenFile
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	router := rpc.NewRouter()
	agent.New().Register(router)
	logs := &syncBuf{}
	srv, err := New(Options{Config: &cfg, Router: router, Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		sock:   cfg.Socket.Path,
		wsURL:  "ws://" + srv.WebSocketAddr() + cfg.WebSocket.Path,
		cancel: cancel,
		done:   make(chan struct{}),
		logs:   logs,
	}
	go func() { h.err = srv.Serve(ctx); close(h.done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})
	return h
}

// client is an rpc.Peer talking to the server that collects heartbeat updates.
type client struct {
	p       *rpc.Peer
	updates chan rpc.Update
}

func newClient(t *testing.T, conn rpc.Conn) *client {
	t.Helper()
	c := &client{updates: make(chan rpc.Update, 64)}
	r := rpc.NewRouter()
	r.Handle(agent.NotifyHeartbeat, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var u rpc.Update
		if err := json.Unmarshal(params, &u); err != nil {
			return nil, err
		}
		c.updates <- u
		return nil, nil
	})
	c.p = rpc.NewPeer(conn, r)
	go c.p.Run(context.Background())
	t.Cleanup(func() { c.p.Close() })
	return c
}

func (c *client) call(t *testing.T, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.p.Call(ctx, method, params, result)
}

func wantCode(t *testing.T, err error, code int) {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want rpc error %d", err, code)
	}
}

func dialAll(t *testing.T, h *harness) map[string]*client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uc, err := unixsock.Dial(ctx, h.sock)
	if err != nil {
		t.Fatal(err)
	}
	wc, err := ws.Dial(ctx, h.wsURL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*client{"unix": newClient(t, uc), "websocket": newClient(t, wc)}
}

func TestEndToEnd(t *testing.T) {
	h := start(t)
	for name, c := range dialAll(t, h) {
		t.Run(name, func(t *testing.T) {
			wantCode(t, c.call(t, "agent.info", nil, nil), rpc.CodeHandshakeRequired)
			wantCode(t, c.call(t, "agent.heartbeat.subscribe", nil, nil), rpc.CodeHandshakeRequired)

			// rpc.discover is allowed before the handshake.
			var doc struct {
				Info struct {
					Version string `json:"version"`
				} `json:"info"`
			}
			if err := c.call(t, "rpc.discover", nil, &doc); err != nil || doc.Info.Version != api.ProtocolVersion {
				t.Fatalf("discover = %+v, %v", doc, err)
			}

			wantCode(t, c.call(t, "agent.hello", map[string]any{"protocol": "nope"}, nil), rpc.CodeInvalidParams)
			wantCode(t, c.call(t, "agent.info", nil, nil), rpc.CodeHandshakeRequired)

			var hello struct {
				Protocol string `json:"protocol"`
				Agent    struct{ Name string }
			}
			err := c.call(t, "agent.hello", map[string]any{
				"protocol": "1.7.3", "client": map[string]string{"name": "test", "version": "0"},
			}, &hello)
			if err != nil || hello.Protocol != api.ProtocolVersion || hello.Agent.Name != "ki-agent" {
				t.Fatalf("hello = %+v, %v", hello, err)
			}

			var info struct {
				Hostname string `json:"hostname"`
				OS       string `json:"os"`
			}
			if err := c.call(t, "agent.info", nil, &info); err != nil || info.OS == "" {
				t.Fatalf("info = %+v, %v", info, err)
			}
			var pong struct{ Pong bool }
			if err := c.call(t, "agent.ping", nil, &pong); err != nil || !pong.Pong {
				t.Fatalf("ping = %+v, %v", pong, err)
			}

			var sub struct {
				Subscription string `json:"subscription"`
				IntervalMS   int    `json:"interval_ms"`
			}
			if err := c.call(t, "agent.heartbeat.subscribe", map[string]any{"interval_ms": 1}, &sub); err != nil {
				t.Fatal(err)
			}
			if sub.IntervalMS != 250 {
				t.Fatalf("interval_ms = %d, want clamped 250", sub.IntervalMS)
			}
			var last uint64
			for range 2 {
				select {
				case u := <-c.updates:
					if u.Subscription != sub.Subscription || u.Seq <= last || len(u.Data) == 0 {
						t.Fatalf("bad update %+v (last seq %d)", u, last)
					}
					last = u.Seq
				case <-time.After(3 * time.Second):
					t.Fatal("timed out waiting for heartbeat")
				}
			}

			if err := c.call(t, "agent.heartbeat.unsubscribe", map[string]any{"subscription": sub.Subscription}, nil); err != nil {
				t.Fatal(err)
			}
			wantCode(t, c.call(t, "agent.heartbeat.unsubscribe", map[string]any{"subscription": sub.Subscription}, nil), rpc.CodeSubscriptionNotFound)
			wantCode(t, c.call(t, "agent.heartbeat.unsubscribe", nil, nil), rpc.CodeInvalidParams)
		})
	}
}

func rawConns(t *testing.T, h *harness) map[string]rpc.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	uc, err := unixsock.Dial(ctx, h.sock)
	if err != nil {
		t.Fatal(err)
	}
	wc, err := ws.Dial(ctx, h.wsURL, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { uc.Close(); wc.Close() })
	return map[string]rpc.Conn{"unix": uc, "websocket": wc}
}

func readOne(t *testing.T, c rpc.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestPipelinedAfterHello(t *testing.T) {
	h := start(t)
	for name, c := range rawConns(t, h) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// Sent before hello: rejected. Then hello, info and ping back to back.
			for i, m := range []string{"agent.ping", "agent.hello", "agent.info", "agent.ping"} {
				params := ""
				if m == "agent.hello" {
					params = `,"params":{"protocol":"1.0.0"}`
				}
				msg := `{"jsonrpc":"2.0","id":` + string(rune('1'+i)) + `,"method":"` + m + `"` + params + `}`
				if err := c.Write(ctx, []byte(msg)); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 4 {
				got := readOne(t, c)
				wantErr := i == 0
				if strings.Contains(got, `"error"`) != wantErr {
					t.Fatalf("response %d = %s (wantErr %v)", i+1, got, wantErr)
				}
				if wantErr && !strings.Contains(got, "-32002") {
					t.Fatalf("response 1 = %s, want -32002", got)
				}
			}
		})
	}
}

func TestIncompatibleMajorClosesConnection(t *testing.T) {
	h := start(t)
	for name, c := range rawConns(t, h) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Write(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"agent.hello","params":{"protocol":"2.0.0"}}`)); err != nil {
				t.Fatal(err)
			}
			if got := readOne(t, c); !strings.Contains(got, "-32003") {
				t.Fatalf("response = %s, want -32003", got)
			}
			if b, err := c.Read(ctx); err == nil {
				t.Fatalf("connection still open, got %s", b)
			}
		})
	}
}

func TestWebSocketRejectsBadToken(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ws.Dial(ctx, h.wsURL, "wrong"); err == nil {
		t.Fatal("expected dial with wrong token to fail")
	}
	req, _ := http.NewRequest(http.MethodGet, strings.Replace(h.wsURL, "ws://", "http://", 1), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}

func TestGracefulShutdownWithOpenSubscription(t *testing.T) {
	h := start(t)
	clients := dialAll(t, h)
	for _, c := range clients {
		if err := c.call(t, "agent.hello", map[string]any{"protocol": "1.0.0"}, nil); err != nil {
			t.Fatal(err)
		}
		if err := c.call(t, "agent.heartbeat.subscribe", map[string]any{"interval_ms": 250}, nil); err != nil {
			t.Fatal(err)
		}
	}

	h.cancel()
	select {
	case <-h.done:
		if h.err != nil {
			t.Fatalf("Serve = %v, want nil", h.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown did not complete promptly")
	}
	if _, err := os.Stat(h.sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket file should be removed, stat err = %v", err)
	}
	for name, c := range clients {
		select {
		case <-c.p.Done():
		case <-time.After(2 * time.Second):
			t.Fatalf("%s client connection still open after shutdown", name)
		}
	}
	logs := h.logs.String()
	if !strings.Contains(logs, `"connection opened"`) || !strings.Contains(logs, `"connection closed"`) {
		t.Fatalf("connection lifecycle not logged:\n%s", logs)
	}
	if strings.Contains(logs, testToken) {
		t.Fatal("token leaked into logs")
	}
}

func TestNewRejectsMissingToken(t *testing.T) {
	cfg := config.Default()
	cfg.WebSocket.Listen = "127.0.0.1:0"
	_, err := New(Options{Config: &cfg, Router: rpc.NewRouter(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil {
		t.Fatal("expected error for websocket without token file")
	}
}
