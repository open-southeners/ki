package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

func serve(t *testing.T, opts Options, onConn func(context.Context, rpc.Conn)) *httptest.Server {
	t.Helper()
	h, err := NewHandler(opts, onConn)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func echo(ctx context.Context, c rpc.Conn) {
	for {
		m, err := c.Read(ctx)
		if err != nil {
			return
		}
		if c.Write(ctx, m) != nil {
			return
		}
	}
}

func TestEmptyTokenRefused(t *testing.T) {
	if _, err := NewHandler(Options{}, echo); err == nil {
		t.Fatal("expected error")
	}
}

func TestRoundTrip(t *testing.T) {
	s := serve(t, Options{Token: "secret"}, echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, s.URL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, m := range []string{`{"a":1}`, `[{"a":1}]`} {
		if err := c.Write(ctx, []byte(m)); err != nil {
			t.Fatal(err)
		}
		got, err := c.Read(ctx)
		if err != nil || string(got) != m {
			t.Fatalf("got %q err %v", got, err)
		}
	}
}

func TestUnauthorized(t *testing.T) {
	s := serve(t, Options{Token: "secret"}, echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for name, hdr := range map[string]string{"missing": "", "wrong": "Bearer nope", "scheme": "secret", "prefix": "Bearer secret2"} {
		h := http.Header{}
		if hdr != "" {
			h.Set("Authorization", hdr)
		}
		_, resp, err := websocket.Dial(ctx, s.URL, &websocket.DialOptions{HTTPHeader: h})
		if err == nil {
			t.Fatalf("%s: expected failure", name)
		}
		if resp == nil || resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: resp = %v", name, resp)
		}
	}
	if _, err := Dial(ctx, s.URL, "bad"); err == nil {
		t.Fatal("Dial with bad token succeeded")
	}
}

func TestReadLimit(t *testing.T) {
	s := serve(t, Options{Token: "t", ReadLimit: 64}, echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, s.URL, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Write(ctx, make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Read(ctx); err == nil {
		t.Fatal("expected close for oversized message")
	}
}

// A peer that never reads never answers pings, so the server must drop it.
func TestHeartbeatTimeout(t *testing.T) {
	done := make(chan error, 1)
	s := serve(t, Options{Token: "t", HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond},
		func(ctx context.Context, c rpc.Conn) {
			_, err := c.Read(ctx)
			done <- err
		})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, s.URL, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected read error")
		}
	case <-ctx.Done():
		t.Fatal("server did not drop unresponsive peer")
	}
}

// A peer that keeps reading answers pings and stays connected.
func TestHeartbeatHealthyPeerSurvives(t *testing.T) {
	s := serve(t, Options{Token: "t", HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond}, echo)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, s.URL, "t")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go func() { // keep reading so pongs are processed
		time.Sleep(400 * time.Millisecond)
		c.Write(ctx, []byte(`{}`))
	}()
	got, err := c.Read(ctx)
	if err != nil || string(got) != "{}" {
		t.Fatalf("got %q err %v", got, err)
	}
}
