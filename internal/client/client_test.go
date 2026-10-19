package client

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/unixsock"
)

// testServer serves router on a temp socket, one peer per connection.
func testServer(t *testing.T, router *rpc.Router) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.sock")
	ln, err := unixsock.Listen(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept(ctx)
			if err != nil {
				return
			}
			p := rpc.NewPeer(conn, router)
			wg.Add(1)
			go func() { defer wg.Done(); _ = p.Run(ctx) }()
		}
	}()
	t.Cleanup(func() { cancel(); _ = ln.Close(); wg.Wait() })
	return path
}

func hello(r *rpc.Router) {
	r.Handle("agent.hello", func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var in struct {
			Protocol string `json:"protocol"`
			Client   struct{ Name string }
		}
		if err := rpc.DecodeParams(params, &in); err != nil {
			return nil, err
		}
		if in.Protocol == "" || in.Client.Name != "ki-agent-cli" {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "bad hello")
		}
		return map[string]any{"protocol": in.Protocol}, nil
	})
}

func TestCallAndError(t *testing.T) {
	r := rpc.NewRouter()
	hello(r)
	r.Handle("x.echo", func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		return json.RawMessage(params), nil
	})
	r.Handle("x.fail", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, rpc.NewError(rpc.CodeNotFound, "project not found")
	})
	c, err := Dial(context.Background(), testServer(t, r))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var out struct{ A int }
	if err := c.Call(context.Background(), "x.echo", map[string]int{"A": 7}, &out); err != nil || out.A != 7 {
		t.Fatalf("echo = %+v, %v", out, err)
	}
	var re *rpc.Error
	if err := c.Call(context.Background(), "x.fail", nil, nil); !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestDialFailures(t *testing.T) {
	if _, err := Dial(context.Background(), filepath.Join(t.TempDir(), "none.sock")); err == nil {
		t.Fatal("expected dial error")
	}
	// A server that rejects agent.hello.
	r := rpc.NewRouter()
	r.Handle("agent.hello", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, rpc.NewError(rpc.CodeIncompatibleVersion, "incompatible")
	})
	if _, err := Dial(context.Background(), testServer(t, r)); err == nil {
		t.Fatal("expected handshake error")
	}
}

// subServer registers x.subscribe, which sends n updates for a fresh id
// before its response (so they precede it on the wire) and then eof.
func subServer(t *testing.T, n int, withResponseDelay bool) string {
	r := rpc.NewRouter()
	hello(r)
	r.Handle("x.subscribe", func(ctx context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		id := "sub-1"
		for i := 1; i <= n; i++ {
			err := p.Notify(ctx, "x.update", rpc.Update{Subscription: id, Seq: uint64(i), Data: json.RawMessage(`{"n":` + itoa(i) + `}`)})
			if err != nil {
				return nil, err
			}
		}
		if withResponseDelay {
			time.Sleep(50 * time.Millisecond)
		}
		return map[string]string{"subscription": id}, nil
	})
	r.Handle("x.unsubscribe", func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		return struct{}{}, nil
	})
	return testServer(t, r)
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestSubscribeOrderedAndEarlyUpdates(t *testing.T) {
	const n = 200
	for _, delay := range []bool{false, true} {
		sock := subServer(t, n, delay)
		c, err := Dial(context.Background(), sock)
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		sub, err := c.Subscribe(context.Background(), "x.subscribe", nil, "x.update", func(seq int64, data json.RawMessage) bool {
			got = append(got, seq)
			return seq < n
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-sub.Done():
		case <-time.After(5 * time.Second):
			t.Fatalf("timeout after %d updates", len(got))
		}
		if len(got) != n {
			t.Fatalf("delay=%v: got %d updates", delay, len(got))
		}
		for i, s := range got {
			if s != int64(i+1) {
				t.Fatalf("delay=%v: out of order at %d: %d", delay, i, s)
			}
		}
		c.Close()
	}
}

func TestSubscribeSkipsPermanentGap(t *testing.T) {
	r := rpc.NewRouter()
	hello(r)
	r.Handle("x.subscribe", func(ctx context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		for _, seq := range []uint64{1, 3, 4} { // seq 2 never arrives (snapshot semantics)
			_ = p.Notify(ctx, "x.update", rpc.Update{Subscription: "s", Seq: seq, Data: json.RawMessage(`1`)})
		}
		return map[string]string{"subscription": "s"}, nil
	})
	c, err := Dial(context.Background(), testServer(t, r))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var mu sync.Mutex
	var got []int64
	_, err = c.Subscribe(context.Background(), "x.subscribe", nil, "x.update", func(seq int64, _ json.RawMessage) bool {
		mu.Lock()
		got = append(got, seq)
		mu.Unlock()
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestSubscriptionClose(t *testing.T) {
	var mu sync.Mutex
	var unsubscribed string
	r := rpc.NewRouter()
	hello(r)
	r.Handle("x.subscribe", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return map[string]string{"subscription": "abc"}, nil
	})
	r.Handle("x.unsubscribe", func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var in struct{ Subscription string }
		_ = rpc.DecodeParams(params, &in)
		mu.Lock()
		unsubscribed = in.Subscription
		mu.Unlock()
		return struct{}{}, nil
	})
	c, err := Dial(context.Background(), testServer(t, r))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sub, err := c.Subscribe(context.Background(), "x.subscribe", nil, "x.update", func(int64, json.RawMessage) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if err := sub.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-sub.Done()
	mu.Lock()
	defer mu.Unlock()
	if unsubscribed != "abc" {
		t.Fatalf("unsubscribed %q", unsubscribed)
	}
}
