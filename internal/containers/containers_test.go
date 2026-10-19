package containers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
	"github.com/open-southeners/ki-agent/internal/runtime"
)

// fakeRuntime is an in-memory runtime.Runtime.
type fakeRuntime struct {
	mu    sync.Mutex
	err   error // returned by every call when set
	calls []string
	lines []runtime.LogLine
	opts  runtime.LogOptions
	// hold keeps Logs open after the lines until released or ctx ends.
	hold chan struct{}
}

func (f *fakeRuntime) record(s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
	return f.err
}

func (f *fakeRuntime) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeRuntime) id(op, id string) error {
	if err := f.record(op + " " + id); err != nil {
		return err
	}
	switch id {
	case "missing":
		return fmt.Errorf("%w: %s", runtime.ErrNotFound, id)
	case "foreign":
		return fmt.Errorf("%w: %s", runtime.ErrNotManaged, id)
	}
	return nil
}

func (f *fakeRuntime) Ping(context.Context) (runtime.Info, error) {
	return runtime.Info{Name: "fake", Version: "1"}, f.record("ping")
}

func (f *fakeRuntime) List(_ context.Context, project string) ([]runtime.Container, error) {
	if err := f.record("list " + project); err != nil {
		return nil, err
	}
	if project == "none" {
		return nil, nil
	}
	return []runtime.Container{{ID: "c1", Name: "web", State: "running"}}, nil
}

func (f *fakeRuntime) Inspect(_ context.Context, id string) (runtime.ContainerDetail, error) {
	if err := f.id("inspect", id); err != nil {
		return runtime.ContainerDetail{}, err
	}
	return runtime.ContainerDetail{Container: runtime.Container{ID: id}, ExitCode: 3}, nil
}

func (f *fakeRuntime) Start(_ context.Context, id string) error { return f.id("start", id) }

func (f *fakeRuntime) Stop(_ context.Context, id string, d time.Duration) error {
	return f.id(fmt.Sprintf("stop(%v)", d), id)
}

func (f *fakeRuntime) Restart(_ context.Context, id string, d time.Duration) error {
	return f.id(fmt.Sprintf("restart(%v)", d), id)
}

func (f *fakeRuntime) Remove(_ context.Context, id string, force bool) error {
	return f.id(fmt.Sprintf("remove(%v)", force), id)
}

func (f *fakeRuntime) PruneProject(_ context.Context, project string, volumes bool) (runtime.PruneReport, error) {
	return runtime.PruneReport{}, f.record(fmt.Sprintf("prune(%v) %s", volumes, project))
}

func (f *fakeRuntime) Logs(ctx context.Context, id string, opts runtime.LogOptions, fn func(runtime.LogLine) error) error {
	f.mu.Lock()
	f.opts = opts
	lines, hold := f.lines, f.hold
	f.mu.Unlock()
	if err := f.id("logs", id); err != nil {
		return err
	}
	for _, l := range lines {
		if err := fn(l); err != nil {
			return err
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type testClient struct {
	p       *rpc.Peer
	updates chan rpc.Update
}

func setup(t *testing.T, rt runtime.Runtime) *testClient {
	t.Helper()
	srv := rpc.NewRouter()
	New(rt, nil).Register(srv)
	a, b := rpctest.Pipe()

	tc := &testClient{updates: make(chan rpc.Update, 64)}
	cr := rpc.NewRouter()
	cr.Handle(NotifyLogs, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var u rpc.Update
		if err := json.Unmarshal(params, &u); err != nil {
			return nil, err
		}
		tc.updates <- u
		return nil, nil
	})
	sp := rpc.NewPeer(a, srv)
	tc.p = rpc.NewPeer(b, cr)
	go sp.Run(context.Background())
	go tc.p.Run(context.Background())
	t.Cleanup(func() { tc.p.Close(); sp.Close() })
	return tc
}

func (c *testClient) call(t *testing.T, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.p.Call(ctx, method, params, result)
}

func (c *testClient) next(t *testing.T) rpc.Update {
	t.Helper()
	select {
	case u := <-c.updates:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for update")
		return rpc.Update{}
	}
}

func wantCode(t *testing.T, err error, code int) {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want rpc error %d", err, code)
	}
}

type m = map[string]any

func TestSimpleMethods(t *testing.T) {
	f := &fakeRuntime{}
	c := setup(t, f)

	var info runtime.Info
	if err := c.call(t, MethodRuntimeInfo, nil, &info); err != nil || info.Name != "fake" {
		t.Fatalf("info = %+v, %v", info, err)
	}
	var list []runtime.Container
	if err := c.call(t, MethodList, m{"project": "shop"}, &list); err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	var raw json.RawMessage
	if err := c.call(t, MethodList, m{"project": "none"}, &raw); err != nil || string(raw) != "[]" {
		t.Fatalf("empty list = %s, %v (want [])", raw, err)
	}
	var det runtime.ContainerDetail
	if err := c.call(t, MethodInspect, m{"id": "c1"}, &det); err != nil || det.ID != "c1" || det.ExitCode != 3 {
		t.Fatalf("inspect = %+v, %v", det, err)
	}

	for _, tt := range []struct {
		method string
		params m
		want   string
	}{
		{MethodStart, m{"id": "c1"}, "start c1"},
		{MethodStop, m{"id": "c1"}, "stop(-1ns) c1"},
		{MethodStop, m{"id": "c1", "timeout_s": 7}, "stop(7s) c1"},
		{MethodRestart, m{"id": "c1", "timeout_s": 0}, "restart(0s) c1"},
		{MethodRemove, m{"id": "c1"}, "remove(false) c1"},
		{MethodRemove, m{"id": "c1", "force": true}, "remove(true) c1"},
	} {
		var res json.RawMessage
		if err := c.call(t, tt.method, tt.params, &res); err != nil || string(res) != "{}" {
			t.Fatalf("%s: %s, %v", tt.method, res, err)
		}
		calls := f.called()
		if got := calls[len(calls)-1]; got != tt.want {
			t.Errorf("%s: runtime call %q, want %q", tt.method, got, tt.want)
		}
	}
}

func TestParamValidation(t *testing.T) {
	c := setup(t, &fakeRuntime{})
	for _, tt := range []struct {
		method string
		params any
	}{
		{MethodInspect, nil},
		{MethodInspect, m{"id": ""}},
		{MethodStart, m{}},
		{MethodStop, m{"id": "c1", "timeout_s": -1}},
		{MethodRestart, m{"id": "c1", "timeout_s": "x"}},
		{MethodRemove, m{"id": 5}},
		{MethodLogs, m{"id": "c1", "tail": -1}},
		{MethodLogs, m{"id": "c1", "since": "yesterday"}},
		{MethodLogs, m{}},
		{MethodUnlogs, m{}},
	} {
		wantCode(t, c.call(t, tt.method, tt.params, nil), rpc.CodeInvalidParams)
	}
}

func TestErrorMapping(t *testing.T) {
	f := &fakeRuntime{}
	c := setup(t, f)
	methods := []string{MethodInspect, MethodStart, MethodStop, MethodRestart, MethodRemove, MethodLogs}
	var msgs []string
	for _, id := range []string{"missing", "foreign"} {
		for _, method := range methods {
			err := c.call(t, method, m{"id": id}, nil)
			wantCode(t, err, rpc.CodeNotFound)
			var re *rpc.Error
			errors.As(err, &re)
			msgs = append(msgs, re.Message)
		}
	}
	for _, s := range msgs {
		if s != msgs[0] {
			t.Fatalf("not-found and not-managed messages differ: %q vs %q", s, msgs[0])
		}
	}

	f.err = fmt.Errorf("%w: dial refused", runtime.ErrUnavailable)
	for _, method := range append(methods, MethodList, MethodRuntimeInfo) {
		wantCode(t, c.call(t, method, m{"id": "c1"}, nil), rpc.CodeRuntimeUnavailable)
	}

	f.err = errors.New("boom")
	wantCode(t, c.call(t, MethodStart, m{"id": "c1"}, nil), rpc.CodeInternalError)
}

func subscribe(t *testing.T, c *testClient, params m) string {
	t.Helper()
	var res struct {
		Subscription string `json:"subscription"`
	}
	if err := c.call(t, MethodLogs, params, &res); err != nil || res.Subscription == "" {
		t.Fatalf("subscribe = %+v, %v", res, err)
	}
	return res.Subscription
}

func TestLogsStreamToEOF(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := &fakeRuntime{lines: []runtime.LogLine{
		{Stream: "stdout", Time: ts, Text: "one"},
		{Stream: "stderr", Text: "two"},
	}}
	c := setup(t, f)
	id := subscribe(t, c, m{"id": "c1", "timestamps": true, "since": "2026-01-01T00:00:00Z"})

	type data struct {
		Stream string     `json:"stream"`
		Time   *time.Time `json:"time"`
		Text   string     `json:"text"`
		EOF    bool       `json:"eof"`
	}
	// The client dispatches notifications concurrently, so order by seq.
	got := make([]data, 3)
	for range 3 {
		u := c.next(t)
		if u.Subscription != id || u.Seq < 1 || u.Seq > 3 {
			t.Fatalf("update = %+v", u)
		}
		if err := json.Unmarshal(u.Data, &got[u.Seq-1]); err != nil {
			t.Fatal(err)
		}
	}
	if got[0].Text != "one" || got[0].Stream != "stdout" || got[0].Time == nil || !got[0].Time.Equal(ts) {
		t.Errorf("line 1 = %+v", got[0])
	}
	if got[1].Text != "two" || got[1].Stream != "stderr" || got[1].Time != nil {
		t.Errorf("line 2 = %+v", got[1])
	}
	if !got[2].EOF {
		t.Errorf("last = %+v, want eof", got[2])
	}
	f.mu.Lock()
	opts := f.opts
	f.mu.Unlock()
	if !opts.Follow || opts.Tail != DefaultTail || !opts.Timestamps || opts.Since.IsZero() {
		t.Errorf("log options = %+v", opts)
	}

	// The subscription closed itself after eof.
	wantCode(t, c.call(t, MethodUnlogs, m{"subscription": id}, nil), rpc.CodeSubscriptionNotFound)
}

func TestLogsTailClamp(t *testing.T) {
	for _, tt := range []struct {
		tail any
		want int
	}{{nil, DefaultTail}, {0, 0}, {5, 5}, {1 << 20, MaxTail}} {
		f := &fakeRuntime{}
		c := setup(t, f)
		p := m{"id": "c1"}
		if tt.tail != nil {
			p["tail"] = tt.tail
		}
		subscribe(t, c, p)
		c.next(t) // eof
		f.mu.Lock()
		got := f.opts.Tail
		f.mu.Unlock()
		if got != tt.want {
			t.Errorf("tail %v: runtime Tail = %d, want %d", tt.tail, got, tt.want)
		}
	}
}

func TestLogsUnsubscribeStopsStream(t *testing.T) {
	f := &fakeRuntime{hold: make(chan struct{}), lines: []runtime.LogLine{{Stream: "stdout", Text: "x"}}}
	c := setup(t, f)
	id := subscribe(t, c, m{"id": "c1"})
	c.next(t)

	var res json.RawMessage
	if err := c.call(t, MethodUnlogs, m{"subscription": id}, &res); err != nil || string(res) != "{}" {
		t.Fatalf("unsubscribe = %s, %v", res, err)
	}
	// No eof after an explicit unsubscribe, and the runtime stream is cancelled
	// (Logs returns once ctx ends; the held fake would otherwise block forever).
	select {
	case u := <-c.updates:
		t.Fatalf("unexpected update after unsubscribe: %+v", u)
	case <-time.After(200 * time.Millisecond):
	}
}
