package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
	"github.com/open-southeners/ki-agent/internal/runtime"
)

// liveReader returns ever-growing counters for a two-core host.
type liveReader struct {
	mu    sync.Mutex
	n     float64
	reads atomic.Int32
}

func (r *liveReader) Read(context.Context, []string) (HostCounters, error) {
	r.reads.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n += 10
	core := CPUTimes{Total: r.n, Idle: r.n / 2}
	return HostCounters{CPU: CPUTimes{Total: 2 * r.n, Idle: r.n}, PerCore: []CPUTimes{core, core}}, nil
}

// stepClock advances by step on every call, so consecutive samples are
// always distinct in time.
type stepClock struct {
	mu   sync.Mutex
	t    time.Time
	step time.Duration
}

func (c *stepClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(c.step)
	return c.t
}

func testConfig() config.Monitor {
	return config.Monitor{
		HostInterval: time.Hour, ContainerInterval: time.Hour,
		History: time.Hour, HistoryResolution: 15 * time.Second,
	}
}

type fixture struct {
	m      *Manager
	rt     *fakeRuntime
	reader *liveReader
	client *rpc.Peer
	host   chan rpc.Update
	cont   chan rpc.Update
}

// newFixture returns a Manager (not started: tests drive the samplers or call
// Start) wired to an RPC client over a pipe.
func newFixture(t *testing.T, cfg config.Monitor) *fixture {
	t.Helper()
	f := &fixture{rt: &fakeRuntime{}, reader: &liveReader{}, host: make(chan rpc.Update, 64), cont: make(chan rpc.Update, 64)}
	clk := &stepClock{t: t0, step: 20 * time.Second}
	f.m = NewManager(Options{Config: cfg, DataDir: "/data", Runtime: f.rt, HostReader: f.reader, Now: clk.now})
	t.Cleanup(f.m.Close)

	srv := rpc.NewRouter()
	f.m.Register(srv)
	a, b := rpctest.Pipe()
	cr := rpc.NewRouter()
	forward := func(ch chan rpc.Update) rpc.Handler {
		return func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
			var u rpc.Update
			if err := json.Unmarshal(params, &u); err != nil {
				return nil, err
			}
			ch <- u
			return nil, nil
		}
	}
	cr.Handle(NotifyHostUpdate, forward(f.host))
	cr.Handle(NotifyContainersUpdate, forward(f.cont))
	sp := rpc.NewPeer(a, srv)
	f.client = rpc.NewPeer(b, cr)
	go sp.Run(context.Background())
	go f.client.Run(context.Background())
	t.Cleanup(func() { f.client.Close(); sp.Close() })
	return f
}

func (f *fixture) call(t *testing.T, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return f.client.Call(ctx, method, params, result)
}

func nextUpdate(t *testing.T, ch chan rpc.Update) rpc.Update {
	t.Helper()
	select {
	case u := <-ch:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for update")
		return rpc.Update{}
	}
}

func noUpdate(t *testing.T, ch chan rpc.Update, d time.Duration) {
	t.Helper()
	select {
	case u := <-ch:
		t.Fatalf("unexpected update: %s", u.Data)
	case <-time.After(d):
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
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

// sampleBoth takes n host and container samples synchronously.
func (f *fixture) sampleBoth(n int) {
	for i := 0; i < n; i++ {
		f.m.sampleHost(context.Background(), f.m.now())
		f.m.sampleContainers(context.Background(), f.m.now())
	}
}

func (f *fixture) twoContainers() {
	f.rt.advance = true
	f.rt.set([]runtime.Container{running(id1, "app", "web"), running(id2, "blog", "wp")},
		map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1), id2: snap(t0, 1, 1)})
}

func TestHostStats(t *testing.T) {
	f := newFixture(t, testConfig())
	var res struct {
		Current *HostSample  `json:"current"`
		History []HostSample `json:"history"`
	}
	raw := map[string]json.RawMessage{}
	if err := f.call(t, MethodHostStats, nil, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["current"]) != "null" {
		t.Fatalf("current before any sample = %s, want null", raw["current"])
	}
	if _, has := raw["history"]; has {
		t.Fatalf("history present without being requested: %v", raw)
	}

	f.sampleBoth(3)
	if err := f.call(t, MethodHostStats, m{"history": true}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Current == nil || len(res.History) != 3 {
		t.Fatalf("result = %+v", res)
	}
	if res.Current.CPU.Percent == nil || len(res.Current.CPU.PerCore) != 0 {
		t.Fatalf("current = %+v, want a CPU percent and no per-core data", res.Current.CPU)
	}
}

func TestHostStatsPerCore(t *testing.T) {
	f := newFixture(t, testConfig())
	f.m.Start(context.Background())
	eventually(t, "first host sample", func() bool { _, ok := f.m.hostRing.Latest(); return ok })

	var res struct {
		Current *HostSample `json:"current"`
	}
	if err := f.call(t, MethodHostStats, m{"per_core": true}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Current == nil || len(res.Current.CPU.PerCore) != 2 {
		t.Fatalf("current = %+v, want 2 per-core values", res.Current)
	}
	eventually(t, "per-core to switch off again", func() bool { return !f.m.host.perCore.Load() })

	// A plain call strips the breakdown stored in the history.
	res.Current = nil
	if err := f.call(t, MethodHostStats, nil, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Current.CPU.PerCore) != 0 {
		t.Fatalf("per_core leaked into a plain call: %+v", res.Current.CPU)
	}
}

func TestHostSubscribe(t *testing.T) {
	f := newFixture(t, testConfig())
	f.sampleBoth(2)

	var sub struct {
		Subscription string `json:"subscription"`
		IntervalMS   int64  `json:"interval_ms"`
	}
	if err := f.call(t, MethodHostSubscribe, nil, &sub); err != nil {
		t.Fatal(err)
	}
	if sub.Subscription == "" || sub.IntervalMS != 2000 {
		t.Fatalf("subscribe result = %+v, want default 2000ms", sub)
	}
	u := nextUpdate(t, f.host)
	if u.Subscription != sub.Subscription || u.Seq != 1 {
		t.Fatalf("update = %+v", u)
	}
	var data struct {
		Sample HostSample `json:"sample"`
	}
	if err := json.Unmarshal(u.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Sample.Time.IsZero() || len(data.Sample.CPU.PerCore) != 0 {
		t.Fatalf("sample = %+v", data.Sample)
	}

	if err := f.call(t, MethodHostUnsubscribe, m{"subscription": sub.Subscription}, nil); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.call(t, MethodHostUnsubscribe, m{"subscription": sub.Subscription}, nil), rpc.CodeSubscriptionNotFound)
	wantCode(t, f.call(t, MethodHostUnsubscribe, m{}, nil), rpc.CodeInvalidParams)
	eventually(t, "subscriber to deregister", func() bool { return f.m.hostSch.count() == 0 })
}

func TestHostSubscribeIntervalClamp(t *testing.T) {
	f := newFixture(t, testConfig())
	for _, tc := range []struct {
		req  int64
		want int64
	}{{10, 1000}, {5000, 5000}, {10_000_000, 60_000}} {
		var sub struct {
			IntervalMS int64 `json:"interval_ms"`
		}
		if err := f.call(t, MethodHostSubscribe, m{"interval_ms": tc.req}, &sub); err != nil {
			t.Fatal(err)
		}
		if sub.IntervalMS != tc.want {
			t.Errorf("interval_ms %d -> %d, want %d", tc.req, sub.IntervalMS, tc.want)
		}
	}
	wantCode(t, f.call(t, MethodHostSubscribe, m{"interval_ms": -1}, nil), rpc.CodeInvalidParams)
	wantCode(t, f.call(t, MethodContainersSubscribe, m{"interval_ms": -1}, nil), rpc.CodeInvalidParams)
	var sub struct {
		IntervalMS int64 `json:"interval_ms"`
	}
	if err := f.call(t, MethodContainersSubscribe, m{"interval_ms": 100}, &sub); err != nil || sub.IntervalMS != 2000 {
		t.Fatalf("containers clamp = %d, %v; want 2000", sub.IntervalMS, err)
	}
	if err := f.call(t, MethodContainersSubscribe, nil, &sub); err != nil || sub.IntervalMS != 5000 {
		t.Fatalf("containers default = %d, %v; want 5000", sub.IntervalMS, err)
	}
}

func TestHostSubscribeWaitsForFirstSample(t *testing.T) {
	f := newFixture(t, testConfig())
	if err := f.call(t, MethodHostSubscribe, nil, nil); err != nil {
		t.Fatal(err)
	}
	noUpdate(t, f.host, 50*time.Millisecond)
	f.m.sampleHost(context.Background(), f.m.now())
	nextUpdate(t, f.host)
}

func TestPerCoreFollowsSubscribers(t *testing.T) {
	f := newFixture(t, testConfig())
	f.m.hostSch.min = 20 * time.Millisecond // let the test run fast
	f.sampleBoth(1)
	var a, b struct {
		Subscription string `json:"subscription"`
	}
	if err := f.call(t, MethodHostSubscribe, m{"per_core": true, "interval_ms": 20}, &a); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, MethodHostSubscribe, m{"interval_ms": 20}, &b); err != nil {
		t.Fatal(err)
	}
	if !f.m.host.perCore.Load() {
		t.Fatal("per-core sampling should be on while a subscriber wants it")
	}
	// Both subscribers get their first update immediately.
	nextUpdate(t, f.host)
	nextUpdate(t, f.host)
	f.sampleBoth(2)

	// The plain subscriber never sees the breakdown; the per-core one does.
	seenA := false
	for i := 0; i < 20 && !seenA; i++ {
		u := nextUpdate(t, f.host)
		var d struct {
			Sample HostSample `json:"sample"`
		}
		if err := json.Unmarshal(u.Data, &d); err != nil {
			t.Fatal(err)
		}
		if len(d.Sample.CPU.PerCore) > 0 {
			if u.Subscription != a.Subscription {
				t.Fatalf("per-core data sent to the wrong subscription: %s", u.Data)
			}
			seenA = true
		}
	}
	if !seenA {
		t.Fatal("per-core subscriber never received per-core data")
	}

	if err := f.call(t, MethodHostUnsubscribe, m{"subscription": a.Subscription}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "per-core sampling to switch off", func() bool { return !f.m.host.perCore.Load() })
}

func TestIntervalAdaptation(t *testing.T) {
	cfg := testConfig()
	cfg.HostInterval = 30 * time.Second
	f := newFixture(t, cfg)
	var a, b struct {
		Subscription string `json:"subscription"`
	}
	if f.m.hostSch.interval() != 30*time.Second || f.m.contSch.interval() != time.Hour {
		t.Fatal("idle intervals must be the configured ones")
	}
	if err := f.call(t, MethodHostSubscribe, m{"interval_ms": 5000}, &a); err != nil {
		t.Fatal(err)
	}
	if got := f.m.hostSch.interval(); got != 5*time.Second {
		t.Fatalf("interval = %v, want 5s", got)
	}
	if err := f.call(t, MethodHostSubscribe, m{"interval_ms": 1000}, &b); err != nil {
		t.Fatal(err)
	}
	if got := f.m.hostSch.interval(); got != time.Second {
		t.Fatalf("interval = %v, want 1s (smallest wins)", got)
	}
	if err := f.call(t, MethodHostUnsubscribe, m{"subscription": b.Subscription}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "interval to relax", func() bool { return f.m.hostSch.interval() == 5*time.Second })

	// A subscriber slower than the configured interval does not slow sampling.
	if err := f.call(t, MethodHostUnsubscribe, m{"subscription": a.Subscription}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, MethodHostSubscribe, m{"interval_ms": 60000}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.m.hostSch.interval(); got != 30*time.Second {
		t.Fatalf("interval = %v, want 30s", got)
	}

	// Containers follow the same rules with their own floor.
	if err := f.call(t, MethodContainersSubscribe, m{"interval_ms": 3000}, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.m.contSch.interval(); got != 3*time.Second {
		t.Fatalf("container interval = %v, want 3s", got)
	}
}

func TestIntervalReleasedOnPeerClose(t *testing.T) {
	f := newFixture(t, testConfig())
	if err := f.call(t, MethodHostSubscribe, m{"interval_ms": 1000, "per_core": true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.call(t, MethodContainersSubscribe, m{"interval_ms": 2000}, nil); err != nil {
		t.Fatal(err)
	}
	if f.m.hostSch.interval() != time.Second {
		t.Fatal("subscriber not registered")
	}
	f.client.Close()
	eventually(t, "intervals to relax after the peer closed", func() bool {
		return f.m.hostSch.interval() == time.Hour && f.m.contSch.interval() == time.Hour &&
			!f.m.host.perCore.Load()
	})
}

func TestFasterSubscriberReschedulesImmediately(t *testing.T) {
	cfg := testConfig()
	f := newFixture(t, cfg)
	f.m.hostSch.min = 20 * time.Millisecond // let the test run fast
	f.m.Start(context.Background())
	eventually(t, "first host sample", func() bool { return f.reader.reads.Load() >= 1 })
	time.Sleep(30 * time.Millisecond)
	if n := f.reader.reads.Load(); n != 1 {
		t.Fatalf("reads = %d, want 1 with a 1h interval and no subscriber", n)
	}
	if err := f.call(t, MethodHostSubscribe, m{"interval_ms": 20}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sampling at the subscriber's pace", func() bool { return f.reader.reads.Load() >= 4 })
}

func TestContainersStats(t *testing.T) {
	f := newFixture(t, testConfig())
	f.twoContainers()
	f.sampleBoth(2)

	type result struct {
		Available  bool `json:"available"`
		Containers []struct {
			Current *ContainerSample  `json:"current"`
			History []ContainerSample `json:"history"`
		} `json:"containers"`
	}
	var all result
	if err := f.call(t, MethodContainersStats, nil, &all); err != nil {
		t.Fatal(err)
	}
	if !all.Available || len(all.Containers) != 2 || all.Containers[0].Current.ID != id1 {
		t.Fatalf("all = %+v", all)
	}
	if all.Containers[0].History != nil {
		t.Fatal("history returned without being requested")
	}

	var byProject result
	if err := f.call(t, MethodContainersStats, m{"project": "blog", "history": true}, &byProject); err != nil {
		t.Fatal(err)
	}
	if len(byProject.Containers) != 1 || byProject.Containers[0].Current.ID != id2 || len(byProject.Containers[0].History) != 2 {
		t.Fatalf("by project = %+v", byProject)
	}

	var none result
	if err := f.call(t, MethodContainersStats, m{"project": "ghost"}, &none); err != nil || len(none.Containers) != 0 || !none.Available {
		t.Fatalf("unknown project = %+v, %v", none, err)
	}

	// Full id and 12-char prefix both resolve.
	for _, id := range []string{id1, id1[:12]} {
		var one result
		if err := f.call(t, MethodContainersStats, m{"id": id}, &one); err != nil {
			t.Fatal(err)
		}
		if len(one.Containers) != 1 || one.Containers[0].Current.ID != id1 {
			t.Fatalf("id %q = %+v", id, one)
		}
	}
	wantCode(t, f.call(t, MethodContainersStats, m{"id": id1[:11]}, nil), rpc.CodeNotFound)
	wantCode(t, f.call(t, MethodContainersStats, m{"id": "cccccccccccc3333"}, nil), rpc.CodeNotFound)
}

func TestContainersStatsEngineDown(t *testing.T) {
	f := newFixture(t, testConfig())
	f.m.contSch.min = 20 * time.Millisecond // let the test run fast
	f.twoContainers()
	f.sampleBoth(1)
	f.rt.setErr(runtime.ErrUnavailable)
	f.sampleBoth(1)

	var res struct {
		Available  bool              `json:"available"`
		Containers []json.RawMessage `json:"containers"`
	}
	if err := f.call(t, MethodContainersStats, nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Available || res.Containers == nil || len(res.Containers) != 0 {
		t.Fatalf("result = %+v, want available false and an empty list", res)
	}
	res.Available, res.Containers = true, nil
	if err := f.call(t, MethodContainersStats, m{"id": id1}, &res); err != nil || res.Available {
		t.Fatalf("by id while down: %+v, %v", res, err)
	}
	wantCode(t, f.call(t, MethodContainersSubscribe, m{"id": id1}, nil), rpc.CodeRuntimeUnavailable)

	// A subscription sees the engine going down and coming back.
	if err := f.call(t, MethodContainersSubscribe, m{"interval_ms": 20}, nil); err != nil {
		t.Fatal(err)
	}
	u := nextUpdate(t, f.cont)
	if string(u.Data) != `{"available":false,"containers":[]}` {
		t.Fatalf("update while down = %s", u.Data)
	}
	f.rt.setErr(nil)
	f.sampleBoth(1)
	u = nextUpdate(t, f.cont)
	var d struct {
		Available  bool              `json:"available"`
		Containers []ContainerSample `json:"containers"`
	}
	if err := json.Unmarshal(u.Data, &d); err != nil || !d.Available || len(d.Containers) != 2 {
		t.Fatalf("update after recovery = %s (%v)", u.Data, err)
	}
}

func TestContainersStatsSubscribe(t *testing.T) {
	f := newFixture(t, testConfig())
	f.m.contSch.min = 20 * time.Millisecond // let the test run fast
	f.twoContainers()
	f.sampleBoth(1)

	var sub struct {
		Subscription string `json:"subscription"`
	}
	if err := f.call(t, MethodContainersSubscribe, m{"project": "app", "interval_ms": 20}, &sub); err != nil {
		t.Fatal(err)
	}
	u := nextUpdate(t, f.cont)
	if u.Subscription != sub.Subscription || u.Seq != 1 {
		t.Fatalf("update = %+v", u)
	}
	var d struct {
		Available  bool              `json:"available"`
		Containers []ContainerSample `json:"containers"`
	}
	if err := json.Unmarshal(u.Data, &d); err != nil {
		t.Fatal(err)
	}
	if !d.Available || len(d.Containers) != 1 || d.Containers[0].ID != id1 {
		t.Fatalf("update = %s", u.Data)
	}

	// A new sampling round produces a new update; nothing new, nothing sent.
	f.sampleBoth(1)
	nextUpdate(t, f.cont)
	noUpdate(t, f.cont, 50*time.Millisecond)

	wantCode(t, f.call(t, MethodContainersSubscribe, m{"id": "cccccccccccc3333"}, nil), rpc.CodeNotFound)
	if err := f.call(t, MethodContainersUnsubscribe, m{"subscription": sub.Subscription}, nil); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.call(t, MethodContainersUnsubscribe, m{"subscription": sub.Subscription}, nil), rpc.CodeSubscriptionNotFound)
}

func TestManagerWithoutRuntime(t *testing.T) {
	mgr := NewManager(Options{Config: testConfig(), HostReader: &liveReader{}})
	defer mgr.Close()
	mgr.sampleContainers(context.Background(), t0)
	if mgr.cont.Available() {
		t.Fatal("no runtime must mean unavailable")
	}
}

func TestManagerStartClose(t *testing.T) {
	f := newFixture(t, testConfig())
	f.m.Start(context.Background())
	f.m.Start(context.Background()) // idempotent
	eventually(t, "initial samples", func() bool {
		_, ok := f.m.hostRing.Latest()
		return ok && f.m.cont.Gen() > 0
	})
	done := make(chan struct{})
	go func() { f.m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
}

func TestScheduleInterval(t *testing.T) {
	s := newSchedule(15*time.Second, time.Second, time.Minute)
	if s.interval() != 15*time.Second {
		t.Fatal("idle interval")
	}
	id := s.add(2*time.Second, false)
	if s.interval() != 2*time.Second {
		t.Fatal("subscriber interval")
	}
	select {
	case <-s.wake:
	default:
		t.Fatal("add must wake the sampler")
	}
	s.remove(id)
	if s.interval() != 15*time.Second || s.count() != 0 {
		t.Fatal("interval not restored")
	}
	if s.clamp(0) != time.Second || s.clamp(time.Hour) != time.Minute {
		t.Fatal("clamp")
	}
}

func TestTriggerContainersSamplesAtOnce(t *testing.T) {
	f := newFixture(t, testConfig())
	f.twoContainers()
	f.m.Start(context.Background())
	eventually(t, "initial container sample", func() bool { return f.m.cont.Gen() >= 1 })
	time.Sleep(30 * time.Millisecond)
	gen := f.m.cont.Gen()
	if gen != 1 {
		t.Fatalf("gen = %d, want 1 with a 1h interval", gen)
	}
	f.m.TriggerContainers()
	f.m.TriggerContainers() // coalesces, never blocks
	eventually(t, "triggered container sample", func() bool { return f.m.cont.Gen() > gen })
}
