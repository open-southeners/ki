package rpc_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

const timeout = 5 * time.Second

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

// running is a Peer whose Run loop is executing in a goroutine.
type running struct {
	*rpc.Peer
	result chan error
}

func start(t *testing.T, c rpc.Conn, r *rpc.Router, opts ...rpc.Option) *running {
	t.Helper()
	p := rpc.NewPeer(c, r, opts...)
	rn := &running{Peer: p, result: make(chan error, 1)}
	go func() { rn.result <- p.Run(context.Background()) }()
	t.Cleanup(func() {
		p.Close()
		rn.waitExit(t)
	})
	return rn
}

// waitExit asserts that Run returns (so all peer goroutines have exited).
func (r *running) waitExit(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.result:
		r.result <- err
		return err
	case <-time.After(timeout):
		t.Fatal("Run did not return: goroutine leak")
		return nil
	}
}

// pair returns two peers connected in memory.
func pair(t *testing.T, ra, rb *rpc.Router, opts ...rpc.Option) (*running, *running) {
	t.Helper()
	ca, cb := rpctest.Pipe()
	return start(t, ca, ra, opts...), start(t, cb, rb, opts...)
}

// raw returns a peer serving r plus the raw other end of its connection.
func raw(t *testing.T, r *rpc.Router, opts ...rpc.Option) (*running, rpc.Conn) {
	t.Helper()
	ca, cb := rpctest.Pipe()
	t.Cleanup(func() { cb.Close() })
	return start(t, ca, r, opts...), cb
}

// exchange writes payload and returns the next message read.
func exchange(t *testing.T, c rpc.Conn, payload string) string {
	t.Helper()
	ctx := ctxT(t)
	if err := c.Write(ctx, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	return readMsg(t, c)
}

func readMsg(t *testing.T, c rpc.Conn) string {
	t.Helper()
	b, err := c.Read(ctxT(t))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// expectSilence asserts that nothing arrives within a short window.
func expectSilence(t *testing.T, c rpc.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if b, err := c.Read(ctx); err == nil {
		t.Fatalf("unexpected message: %s", b)
	}
}

func echoRouter() *rpc.Router {
	r := rpc.NewRouter()
	r.Handle("echo", func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var v any
		if err := rpc.DecodeParams(params, &v); err != nil {
			return nil, err
		}
		return v, nil
	})
	return r
}

// noLeak fails if peer or subscription goroutines are still alive.
func noLeak(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var sb strings.Builder
		pprof.Lookup("goroutine").WriteTo(&sb, 1)
		s := sb.String()
		if !strings.Contains(s, "rpc.(*Peer).writeLoop") &&
			!strings.Contains(s, "rpc.(*Subscription).pump") &&
			!strings.Contains(s, "rpc.(*Peer).Run") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("leaked goroutines:\n%s", s)
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// rawPipe returns both ends of an in-memory pipe, closing them on cleanup.
func rawPipe(t *testing.T) (rpc.Conn, rpc.Conn) {
	t.Helper()
	a, b := rpctest.Pipe()
	t.Cleanup(func() { b.Close() })
	return a, b
}

// exchangeAsync writes payload and discards the response that follows. It
// reads from c so it must run before any subscription traffic.
func exchangeAsync(t *testing.T, c rpc.Conn, payload string) {
	t.Helper()
	if err := c.Write(ctxT(t), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	readMsg(t, c)
}
