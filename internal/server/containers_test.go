package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/containers"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/transport/unixsock"
)

// stubRuntime serves one managed container "web" with two log lines, then ends
// the stream when release is closed.
type stubRuntime struct {
	runtime.Runtime // unimplemented methods panic if reached
	release         chan struct{}
}

func (s *stubRuntime) List(context.Context, string) ([]runtime.Container, error) {
	return []runtime.Container{{ID: "web", Name: "web", State: "running"}}, nil
}

func (s *stubRuntime) Inspect(_ context.Context, id string) (runtime.ContainerDetail, error) {
	if id != "web" {
		return runtime.ContainerDetail{}, fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	}
	return runtime.ContainerDetail{Container: runtime.Container{ID: id}}, nil
}

func (s *stubRuntime) Logs(ctx context.Context, _ string, _ runtime.LogOptions, fn func(runtime.LogLine) error) error {
	for _, text := range []string{"hello", "world"} {
		if err := fn(runtime.LogLine{Stream: "stdout", Text: text}); err != nil {
			return err
		}
	}
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil
}

// logClient is a peer that collects containers.logs.update notifications.
type logClient struct {
	p *rpc.Peer

	mu      sync.Mutex
	updates []rpc.Update
}

func dialLogClient(t *testing.T, sock string) *logClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := unixsock.Dial(ctx, sock)
	if err != nil {
		t.Fatal(err)
	}
	c := &logClient{}
	r := rpc.NewRouter()
	r.Handle(containers.NotifyLogs, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var u rpc.Update
		if err := json.Unmarshal(params, &u); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.updates = append(c.updates, u)
		c.mu.Unlock()
		return nil, nil
	})
	c.p = rpc.NewPeer(conn, r)
	go c.p.Run(context.Background())
	t.Cleanup(func() { c.p.Close() })
	return c
}

func (c *logClient) call(t *testing.T, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.p.Call(ctx, method, params, result)
}

// wait blocks until n updates have arrived and returns them ordered by seq.
func (c *logClient) wait(t *testing.T, n int) []rpc.Update {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.updates) >= n {
			out := slices.Clone(c.updates)
			c.mu.Unlock()
			slices.SortFunc(out, func(a, b rpc.Update) int { return int(a.Seq) - int(b.Seq) })
			return out
		}
		c.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d updates", n)
	return nil
}

func TestContainersOverUnixSocket(t *testing.T) {
	rt := &stubRuntime{release: make(chan struct{})}
	h := startWith(t, func(r *rpc.Router) { containers.New(rt, nil).Register(r) })
	c := dialLogClient(t, h.sock)

	// Behind the handshake gate.
	wantCode(t, c.call(t, "containers.list", nil, nil), rpc.CodeHandshakeRequired)
	wantCode(t, c.call(t, "containers.logs.subscribe", map[string]any{"id": "web"}, nil), rpc.CodeHandshakeRequired)

	var hello struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := c.call(t, "agent.hello", map[string]any{"protocol": "1.0.0"}, &hello); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(hello.Capabilities, "containers") {
		t.Fatalf("capabilities = %v, want containers", hello.Capabilities)
	}

	var list []runtime.Container
	if err := c.call(t, "containers.list", nil, &list); err != nil || len(list) != 1 || list[0].ID != "web" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	wantCode(t, c.call(t, "containers.inspect", map[string]any{"id": "nope"}, nil), rpc.CodeNotFound)
	wantCode(t, c.call(t, "containers.logs.subscribe", map[string]any{"id": "nope"}, nil), rpc.CodeNotFound)

	var sub struct {
		Subscription string `json:"subscription"`
	}
	if err := c.call(t, "containers.logs.subscribe", map[string]any{"id": "web"}, &sub); err != nil {
		t.Fatal(err)
	}
	ups := c.wait(t, 2)
	if ups[0].Subscription != sub.Subscription || string(ups[0].Data) != `{"stream":"stdout","text":"hello"}` {
		t.Fatalf("first update = %+v (%s)", ups[0], ups[0].Data)
	}

	close(rt.release) // container "exits"
	ups = c.wait(t, 3)
	if string(ups[2].Data) != `{"eof":true}` {
		t.Fatalf("last update = %s, want eof", ups[2].Data)
	}
}
