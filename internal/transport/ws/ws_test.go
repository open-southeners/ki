package ws

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/open-southeners/ki/internal/rpc"
)

func serve(t *testing.T, opts Options, onConn func(context.Context, rpc.Conn, any)) *httptest.Server {
	t.Helper()
	h, err := NewHandler(opts, onConn)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func echo(ctx context.Context, c rpc.Conn, _ any) {
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

func TestNoAuthenticateRefused(t *testing.T) {
	if _, err := NewHandler(Options{}, echo); err == nil {
		t.Fatal("expected error")
	}
}

func TestRoundTrip(t *testing.T) {
	s := serve(t, Options{Authenticate: BearerAuth("secret")}, echo)
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
	s := serve(t, Options{Authenticate: BearerAuth("secret")}, echo)
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

// The value returned by Authenticate reaches onConn, and an Authenticate error
// is a 401 that never upgrades.
func TestAuthenticateValuePassedToOnConn(t *testing.T) {
	got := make(chan any, 1)
	s := serve(t, Options{Authenticate: func(r *http.Request) (any, error) {
		if c, err := r.Cookie("sid"); err == nil && c.Value == "good" {
			return "principal-1", nil
		}
		return nil, ErrUnauthorized
	}}, func(ctx context.Context, c rpc.Conn, authn any) {
		got <- authn
		echo(ctx, c, authn)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := DialWith(ctx, s.URL, DialOptions{Header: http.Header{"Cookie": {"sid=good"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if v := <-got; v != "principal-1" {
		t.Fatalf("authn = %v", v)
	}

	_, resp, err := DialWith(ctx, s.URL, DialOptions{Header: http.Header{"Cookie": {"sid=bad"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %v, %v", resp, err)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("missing WWW-Authenticate")
	}
}

func TestReadLimit(t *testing.T) {
	s := serve(t, Options{Authenticate: BearerAuth("t"), ReadLimit: 64}, echo)
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
	s := serve(t, Options{Authenticate: BearerAuth("t"), HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond},
		func(ctx context.Context, c rpc.Conn, _ any) {
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
	s := serve(t, Options{Authenticate: BearerAuth("t"), HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond}, echo)
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

func TestDialWithSendsHeadersAndExposesRejection(t *testing.T) {
	got := make(chan http.Header, 1)
	s := serve(t, Options{Authenticate: BearerAuth("t")}, echo)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Clone()
		s.Config.Handler.ServeHTTP(w, r)
	}))
	defer proxy.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := DialWith(ctx, proxy.URL, DialOptions{Token: "t", Header: http.Header{"X-Test": {"1"}}})
	if err != nil || resp == nil {
		t.Fatalf("DialWith = %v, %v", resp, err)
	}
	defer c.Close()
	if h := <-got; h.Get("Authorization") != "Bearer t" || h.Get("X-Test") != "1" {
		t.Fatalf("headers = %v", h)
	}

	_, resp, err = DialWith(ctx, s.URL, DialOptions{Token: "wrong"})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 response with error, got %v, %v", resp, err)
	}
}

// With a client heartbeat, a server that never reads (so never pongs) is
// dropped and the pending Read fails.
func TestDialWithHeartbeatDropsDeadServer(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wc, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer wc.CloseNow()
		<-r.Context().Done()
		time.Sleep(time.Second)
	}))
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := DialWith(ctx, s.URL, DialOptions{HeartbeatInterval: 20 * time.Millisecond, HeartbeatTimeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatalf("want read error from heartbeat failure, got %v (ctx %v)", err, ctx.Err())
	}
}
