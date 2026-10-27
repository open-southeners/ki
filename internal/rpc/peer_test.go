package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/rpc/rpctest"
)

func TestCallRoundTrip(t *testing.T) {
	r := echoRouter()
	r.Handle("add", func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var p struct{ A, B int }
		if err := rpc.DecodeParams(params, &p); err != nil {
			return nil, err
		}
		return p.A + p.B, nil
	})
	client, _ := pair(t, nil, r)
	var sum int
	if err := client.Call(ctxT(t), "add", map[string]int{"A": 2, "B": 3}, &sum); err != nil {
		t.Fatal(err)
	}
	if sum != 5 {
		t.Fatalf("sum = %d", sum)
	}
}

func TestCallErrors(t *testing.T) {
	r := rpc.NewRouter()
	r.Handle("typed", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, rpc.NewError(rpc.CodeUnauthorized, "nope").WithData(map[string]int{"x": 1})
	})
	r.Handle("wrapped", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, errors.Join(errors.New("ctx"), rpc.NewError(rpc.CodeOverloaded, "busy"))
	})
	r.Handle("plain", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, errors.New("secret database password")
	})
	r.Handle("params", func(_ context.Context, _ *rpc.Peer, p json.RawMessage) (any, error) {
		var v struct{ N int }
		return nil, rpc.DecodeParams(p, &v)
	})
	r.Handle("panic", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		panic("boom")
	})
	client, _ := pair(t, nil, r)

	tests := []struct {
		method  string
		params  any
		code    int
		message string
	}{
		{"typed", nil, rpc.CodeUnauthorized, "nope"},
		{"wrapped", nil, rpc.CodeOverloaded, "busy"},
		{"plain", nil, rpc.CodeInternalError, "internal error"},
		{"params", map[string]string{"N": "x"}, rpc.CodeInvalidParams, ""},
		{"panic", nil, rpc.CodeInternalError, "internal error"},
		{"missing", nil, rpc.CodeMethodNotFound, ""},
	}
	for _, tc := range tests {
		t.Run(tc.method, func(t *testing.T) {
			err := client.Call(ctxT(t), tc.method, tc.params, nil)
			var re *rpc.Error
			if !errors.As(err, &re) {
				t.Fatalf("err = %v, want *rpc.Error", err)
			}
			if re.Code != tc.code {
				t.Fatalf("code = %d, want %d", re.Code, tc.code)
			}
			if tc.message != "" && re.Message != tc.message {
				t.Fatalf("message = %q, want %q", re.Message, tc.message)
			}
			if strings.Contains(re.Message, "secret") {
				t.Fatal("internal error leaked")
			}
		})
	}
	// The peer survives a handler panic.
	if err := client.Call(ctxT(t), "typed", nil, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestErrorData(t *testing.T) {
	r := rpc.NewRouter()
	r.Handle("typed", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, rpc.NewError(-32001, "nope").WithData(map[string]int{"x": 1})
	})
	client, _ := pair(t, nil, r)
	var re *rpc.Error
	if !errors.As(client.Call(ctxT(t), "typed", nil, nil), &re) || string(re.Data) != `{"x":1}` {
		t.Fatalf("data = %v", re)
	}
}

func TestWireFormat(t *testing.T) {
	r := echoRouter()
	r.Handle("nilresult", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return nil, nil })
	r.Handle("fail", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, rpc.NewError(-32001, "no")
	})
	_, c := raw(t, r)

	tests := []struct{ name, in, want string }{
		{"number id", `{"jsonrpc":"2.0","id":7,"method":"echo","params":[1]}`,
			`{"jsonrpc":"2.0","id":7,"result":[1]}`},
		{"string id", `{"jsonrpc":"2.0","id":"abc","method":"echo","params":{"a":1}}`,
			`{"jsonrpc":"2.0","id":"abc","result":{"a":1}}`},
		{"big id verbatim", `{"jsonrpc":"2.0","id":12345678901234567890123,"method":"echo"}`,
			`{"jsonrpc":"2.0","id":12345678901234567890123,"result":null}`},
		{"float id verbatim", `{"jsonrpc":"2.0","id":1.50,"method":"nilresult"}`,
			`{"jsonrpc":"2.0","id":1.50,"result":null}`},
		{"null id request", `{"jsonrpc":"2.0","id":null,"method":"nilresult"}`,
			`{"jsonrpc":"2.0","id":null,"result":null}`},
		{"error", `{"jsonrpc":"2.0","id":1,"method":"fail"}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"no"}}`},
		{"unknown method", `{"jsonrpc":"2.0","id":2,"method":"nope"}`,
			`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"method not found: nope"}}`},
		{"parse error", `{"jsonrpc":"2.0","method":"echo","id":1`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`},
		{"garbage", `nonsense`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"parse error"}}`},
		{"missing jsonrpc", `{"id":3,"method":"echo"}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"invalid request: \"jsonrpc\" must be \"2.0\""}}`},
		{"wrong version", `{"jsonrpc":"1.0","id":3,"method":"echo"}`,
			`{"jsonrpc":"2.0","id":3,"error":{"code":-32600,"message":"invalid request: \"jsonrpc\" must be \"2.0\""}}`},
		{"non-string method", `{"jsonrpc":"2.0","id":4,"method":1}`,
			`{"jsonrpc":"2.0","id":4,"error":{"code":-32600,"message":"invalid request: method must be a non-empty string"}}`},
		{"bad id type", `{"jsonrpc":"2.0","id":{"a":1},"method":"echo"}`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: id must be a string, number or null"}}`},
		{"bad params type", `{"jsonrpc":"2.0","id":5,"method":"echo","params":"x"}`,
			`{"jsonrpc":"2.0","id":5,"error":{"code":-32600,"message":"invalid request: params must be an object or array"}}`},
		{"not an object", `42`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`},
		{"empty batch", `[]`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request: empty batch"}}`},
		{"empty object", `{}`,
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := exchange(t, c, tc.in); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	var got atomic.Int32
	r := rpc.NewRouter()
	r.Handle("note", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		got.Add(1)
		return "ignored", nil
	})
	r.Handle("badnote", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
		return nil, errors.New("fails")
	})
	_, c := raw(t, r)
	for _, m := range []string{"note", "badnote", "unknown"} {
		if err := c.Write(ctxT(t), []byte(`{"jsonrpc":"2.0","method":"`+m+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	expectSilence(t, c)
	if got.Load() != 1 {
		t.Fatalf("handler ran %d times", got.Load())
	}
}

func TestBatch(t *testing.T) {
	_, c := raw(t, echoRouter())

	t.Run("mixed", func(t *testing.T) {
		in := `[` +
			`{"jsonrpc":"2.0","id":1,"method":"echo","params":["a"]},` +
			`{"jsonrpc":"2.0","method":"echo","params":["note"]},` +
			`{"jsonrpc":"2.0","id":"x","method":"nope"},` +
			`7,` +
			`{"jsonrpc":"2.0","id":2,"method":"echo","params":["b"]}]`
		got := exchange(t, c, in)
		var resps []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(got), &resps); err != nil {
			t.Fatalf("%v: %s", err, got)
		}
		if len(resps) != 4 {
			t.Fatalf("want 4 responses (notification omitted), got %d: %s", len(resps), got)
		}
		byID := map[string]string{}
		for _, r := range resps {
			byID[string(r["id"])] = string(r["result"]) + string(r["error"])
		}
		if byID["1"] != `["a"]` || byID["2"] != `["b"]` {
			t.Fatalf("bad results: %s", got)
		}
		if !strings.Contains(byID[`"x"`], "-32601") || !strings.Contains(byID["null"], "-32600") {
			t.Fatalf("bad errors: %s", got)
		}
	})

	t.Run("all notifications", func(t *testing.T) {
		if err := c.Write(ctxT(t), []byte(`[{"jsonrpc":"2.0","method":"echo"},{"jsonrpc":"2.0","method":"echo"}]`)); err != nil {
			t.Fatal(err)
		}
		expectSilence(t, c)
	})

	t.Run("invalid json", func(t *testing.T) {
		got := exchange(t, c, `[{"jsonrpc":"2.0","method":"echo","id":1},{"jsonrpc"]`)
		if !strings.Contains(got, "-32700") {
			t.Fatal(got)
		}
	})

	t.Run("all invalid", func(t *testing.T) {
		got := exchange(t, c, `[1,2,3]`)
		if strings.Count(got, `"code":-32600`) != 3 || !strings.HasPrefix(got, "[") {
			t.Fatal(got)
		}
	})
}

func TestBidirectional(t *testing.T) {
	mk := func(name string) *rpc.Router {
		r := rpc.NewRouter()
		r.Handle("who", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return name, nil })
		// Calls back to the caller while handling its request.
		r.Handle("callback", func(ctx context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
			var other string
			if err := p.Call(ctx, "who", nil, &other); err != nil {
				return nil, err
			}
			return name + "<-" + other, nil
		})
		return r
	}
	a, b := pair(t, mk("A"), mk("B"))
	var got string
	if err := a.Call(ctxT(t), "callback", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got != "B<-A" {
		t.Fatalf("got %q", got)
	}
	if err := b.Call(ctxT(t), "callback", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got != "A<-B" {
		t.Fatalf("got %q", got)
	}
}

func TestConcurrentCalls(t *testing.T) {
	a, b := pair(t, echoRouter(), echoRouter())
	const n = 50
	errs := make(chan error, 2*n)
	for i := 0; i < n; i++ {
		for _, p := range []*running{a, b} {
			go func() {
				var out []int
				err := p.Call(ctxT(t), "echo", []int{i}, &out)
				if err == nil && (len(out) != 1 || out[0] != i) {
					err = errors.New("response routed to wrong call")
				}
				errs <- err
			}()
		}
	}
	for i := 0; i < 2*n; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestNotify(t *testing.T) {
	got := make(chan string, 1)
	r := rpc.NewRouter()
	r.Handle("ev", func(_ context.Context, _ *rpc.Peer, p json.RawMessage) (any, error) {
		got <- string(p)
		return nil, nil
	})
	a, _ := pair(t, nil, r)
	if err := a.Notify(ctxT(t), "ev", map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != `{"n":1}` {
			t.Fatal(s)
		}
	case <-time.After(timeout):
		t.Fatal("no notification")
	}
	if err := a.Notify(ctxT(t), "ev", 5); err == nil {
		t.Fatal("scalar params must be rejected")
	}
}

func TestCallContextCancel(t *testing.T) {
	block := make(chan struct{})
	r := rpc.NewRouter()
	r.Handle("block", func(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil, nil
	})
	a, _ := pair(t, nil, r)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Call(ctx, "block", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(block) // late response must be dropped harmlessly
	var s string
	if err := a.Call(ctxT(t), "nope", nil, &s); err == nil {
		t.Fatal("expected method not found")
	}
}

func TestCloseFailsPendingCallsAndStopsGoroutines(t *testing.T) {
	r := rpc.NewRouter()
	started := make(chan struct{})
	r.Handle("block", func(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done() // handler ctx must be cancelled on peer close
		return nil, ctx.Err()
	})
	ca, cb := rpctest.Pipe()
	server := start(t, cb, r)
	client := start(t, ca, nil)

	res := make(chan error, 1)
	go func() { res <- client.Call(context.Background(), "block", nil, nil) }()
	<-started
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-res:
		if !errors.Is(err, rpc.ErrClosed) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("Call not released by Close")
	}
	if err := client.waitExit(t); err != nil {
		t.Fatalf("client Run = %v", err)
	}
	// The server sees the connection end and shuts down too.
	if err := server.waitExit(t); err != nil {
		t.Fatalf("server Run = %v", err)
	}
	select {
	case <-server.Done():
	default:
		t.Fatal("Done not closed")
	}
	noLeak(t)
	if err := client.Call(ctxT(t), "x", nil, nil); !errors.Is(err, rpc.ErrClosed) {
		t.Fatalf("Call after close = %v", err)
	}
	if err := client.Notify(ctxT(t), "x", nil); !errors.Is(err, rpc.ErrClosed) {
		t.Fatalf("Notify after close = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

type failConn struct {
	rpc.Conn
	err error
}

func (f failConn) Read(context.Context) ([]byte, error) { return nil, f.err }

func TestConnReadErrorEndsRun(t *testing.T) {
	boom := errors.New("boom")
	p := rpc.NewPeer(failConn{Conn: nopConn{}, err: boom}, nil)
	res := make(chan error, 1)
	go func() { res <- p.Run(context.Background()) }()
	select {
	case err := <-res:
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("Run did not return")
	}
	noLeak(t)

	p = rpc.NewPeer(failConn{Conn: nopConn{}, err: io.EOF}, nil)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("EOF should be clean, got %v", err)
	}
	noLeak(t)
}

type nopConn struct{}

func (nopConn) Read(context.Context) ([]byte, error) { return nil, io.EOF }
func (nopConn) Write(context.Context, []byte) error  { return nil }
func (nopConn) Close() error                         { return nil }

func TestRunContextCancel(t *testing.T) {
	ca, _ := rpctest.Pipe()
	p := rpc.NewPeer(ca, nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	go func() { res <- p.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-res:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(timeout):
		t.Fatal("Run did not return")
	}
	noLeak(t)
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("second Run should fail")
	}
}

func TestRemoteCloseEndsRun(t *testing.T) {
	ca, cb := rpctest.Pipe()
	p := start(t, ca, nil)
	cb.Close()
	if err := p.waitExit(t); err != nil {
		t.Fatalf("err = %v", err)
	}
	noLeak(t)
}

// slowConn blocks Writes until released, after letting the first pass writes
// through, to exercise queue back-pressure.
type slowConn struct {
	rpc.Conn
	release chan struct{}
	pass    atomic.Int32
}

func (s *slowConn) Write(ctx context.Context, m []byte) error {
	if s.pass.Add(-1) >= 0 {
		return s.Conn.Write(ctx, m)
	}
	select {
	case <-s.release:
		return s.Conn.Write(ctx, m)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestSendQueueBackpressure(t *testing.T) {
	ca, cb := rpctest.Pipe()
	defer cb.Close()
	sc := &slowConn{Conn: ca, release: make(chan struct{})}
	p := start(t, sc, nil, rpc.WithSendQueue(2))
	// One message is held by the blocked writer, two sit in the queue.
	for i := 0; i < 3; i++ {
		if err := p.Notify(ctxT(t), "n", nil); err != nil {
			t.Fatal(err)
		}
	}
	// The next send must block until its ctx expires.
	deadline := 20
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := p.Notify(ctx, "n", nil)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			break
		}
		if err != nil || i > deadline {
			t.Fatalf("expected back-pressure, err=%v after %d sends", err, i)
		}
	}
	close(sc.release)
}

func TestPeerState(t *testing.T) {
	r := rpc.NewRouter()
	r.Use(func(_ string, next rpc.Handler) rpc.Handler {
		return func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			if ok, _ := p.Get("hello"); ok != true && !strings.Contains(string(params), "hello") {
				return nil, rpc.NewError(rpc.CodeHandshakeRequired, "say hello")
			}
			return next(ctx, p, params)
		}
	})
	r.Handle("secret", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return "ok", nil })
	r.Handle("hello", func(_ context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		p.Set("hello", true)
		return nil, nil
	})
	a, _ := pair(t, nil, r)
	var re *rpc.Error
	if err := a.Call(ctxT(t), "secret", nil, nil); !errors.As(err, &re) || re.Code != rpc.CodeHandshakeRequired {
		t.Fatalf("err = %v", err)
	}
	if err := a.Call(ctxT(t), "hello", []string{"hello"}, nil); err != nil {
		t.Fatal(err)
	}
	var out string
	if err := a.Call(ctxT(t), "secret", nil, &out); err != nil || out != "ok" {
		t.Fatalf("after hello: %v %q", err, out)
	}
}

func TestMiddlewareOrderAndMethods(t *testing.T) {
	var order []string
	r := rpc.NewRouter()
	mw := func(name string) rpc.Middleware {
		return func(_ string, next rpc.Handler) rpc.Handler {
			return func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
				order = append(order, name)
				return next(ctx, p, params)
			}
		}
	}
	r.Use(mw("outer"))
	r.Handle("b", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return 1, nil })
	r.Handle("a", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return 1, nil })
	r.Use(mw("inner")) // applies to already-registered handlers
	if got := strings.Join(r.Methods(), ","); got != "a,b" {
		t.Fatalf("Methods = %s", got)
	}
	c, _ := pair(t, nil, r)
	if err := c.Call(ctxT(t), "a", nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "outer,inner" {
		t.Fatalf("order = %v", order)
	}
}

func TestHandlePanicsOnMisuse(t *testing.T) {
	h := func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return nil, nil }
	r := rpc.NewRouter()
	r.Handle("x", h)
	for name, f := range map[string]func(){
		"duplicate": func() { r.Handle("x", h) },
		"empty":     func() { r.Handle("", h) },
		"nil":       func() { r.Handle("y", nil) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected panic", name)
				}
			}()
			f()
		}()
	}
}

func TestIgnoresStrayResponses(t *testing.T) {
	_, c := raw(t, echoRouter())
	if err := c.Write(ctxT(t), []byte(`{"jsonrpc":"2.0","id":99,"result":1}`)); err != nil {
		t.Fatal(err)
	}
	expectSilence(t, c)
	// And the peer is still healthy.
	got := exchange(t, c, `{"jsonrpc":"2.0","id":1,"method":"echo","params":[1]}`)
	if got != `{"jsonrpc":"2.0","id":1,"result":[1]}` {
		t.Fatal(got)
	}
}

func TestSerialDispatchUntilPredicateFalse(t *testing.T) {
	r := rpc.NewRouter()
	r.Handle("hello", func(_ context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		time.Sleep(50 * time.Millisecond) // a concurrent "check" would overtake this
		p.Set("hello", true)
		return "hi", nil
	})
	r.Handle("check", func(_ context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		if ok, _ := p.Get("hello"); ok != true {
			return nil, rpc.NewError(rpc.CodeHandshakeRequired, "no hello")
		}
		return "ok", nil
	})
	_, c := raw(t, r, rpc.WithSerialDispatch(func(p *rpc.Peer) bool {
		ok, _ := p.Get("hello")
		return ok != true
	}))
	ctx := ctxT(t)
	for _, m := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"hello"}`,
		`{"jsonrpc":"2.0","id":2,"method":"check"}`,
		`{"jsonrpc":"2.0","id":3,"method":"check"}`,
	} {
		if err := c.Write(ctx, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	for range 3 { // after the handshake dispatch is concurrent, so order is free
		got := readMsg(t, c)
		if strings.Contains(got, `"error"`) {
			t.Fatalf("unexpected error: %s", got)
		}
		for id := 1; id <= 3; id++ {
			if strings.Contains(got, `"id":`+itoa(id)+`,`) {
				seen[id] = true
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("responses seen for ids %v, want 1-3", seen)
	}
}

func TestCloseAfterResponseFlushesFirst(t *testing.T) {
	r := rpc.NewRouter()
	r.Handle("bye", func(_ context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		p.CloseAfterResponse()
		return nil, rpc.NewError(rpc.CodeIncompatibleVersion, "go away")
	})
	run, c := raw(t, r)
	got := exchange(t, c, `{"jsonrpc":"2.0","id":1,"method":"bye"}`)
	if !strings.Contains(got, "go away") {
		t.Fatalf("response = %s", got)
	}
	if _, err := c.Read(ctxT(t)); err == nil {
		t.Fatal("expected connection to be closed after the response")
	}
	if err := run.waitExit(t); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}
