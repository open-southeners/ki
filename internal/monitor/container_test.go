package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/runtime"
)

// fakeRuntime is a scriptable StatsRuntime.
type fakeRuntime struct {
	mu         sync.Mutex
	containers []runtime.Container
	stats      map[string]runtime.StatsSnapshot
	listErr    error
	inflight   atomic.Int32
	maxFlight  atomic.Int32
	statsDelay time.Duration
	statsCalls atomic.Int32
	// advance makes every Stats call move the stored snapshot forward by one
	// second of engine time (a fully busy 4-core host at 25 % for the container).
	advance bool
}

func (f *fakeRuntime) List(_ context.Context, project string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []runtime.Container
	for _, c := range f.containers {
		if project == "" || c.Project == project {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeRuntime) Stats(ctx context.Context, id string) (runtime.StatsSnapshot, error) {
	f.statsCalls.Add(1)
	n := f.inflight.Add(1)
	defer f.inflight.Add(-1)
	for {
		old := f.maxFlight.Load()
		if n <= old || f.maxFlight.CompareAndSwap(old, n) {
			break
		}
	}
	if f.statsDelay > 0 {
		select {
		case <-time.After(f.statsDelay):
		case <-ctx.Done():
			return runtime.StatsSnapshot{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	sn := f.stats[id]
	if f.advance && !sn.Read.IsZero() {
		sn.Read = sn.Read.Add(time.Second)
		sn.CPUTotalNs += 1e9
		sn.SystemCPUNs += 4e9
		f.stats[id] = sn
	}
	return sn, nil
}

func (f *fakeRuntime) set(cs []runtime.Container, stats map[string]runtime.StatsSnapshot) {
	f.mu.Lock()
	f.containers, f.stats = cs, stats
	f.mu.Unlock()
}

func (f *fakeRuntime) setErr(err error) {
	f.mu.Lock()
	f.listErr = err
	f.mu.Unlock()
}

const id1 = "aaaaaaaaaaaa1111111111111111111111111111111111111111111111111111"
const id2 = "bbbbbbbbbbbb2222222222222222222222222222222222222222222222222222"

func running(id, project, service string) runtime.Container {
	return runtime.Container{ID: id, Name: "/" + service + "-1", State: "running", Project: project, Service: service}
}

func snap(read time.Time, cpu, sys uint64) runtime.StatsSnapshot {
	return runtime.StatsSnapshot{Read: read, CPUTotalNs: cpu, SystemCPUNs: sys, OnlineCPUs: 4, MemLimit: 400 << 20}
}

func newSampler(rt StatsRuntime) *ContainerSampler {
	return NewContainerSampler(ContainerSamplerOptions{Runtime: rt, History: time.Hour, Resolution: 15 * time.Second})
}

func one(t *testing.T, s *ContainerSampler, id string) ContainerSample {
	t.Helper()
	cs := s.Current("", id)
	if len(cs) != 1 {
		t.Fatalf("Current(%q) = %d samples, want 1", id, len(cs))
	}
	return cs[0]
}

func TestContainerSamplerRates(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	ctx := context.Background()
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1e9, 10e9)})
	if err := s.Sample(ctx, t0); err != nil {
		t.Fatal(err)
	}
	first := one(t, s, "")
	if first.CPU != nil || first.Network.RxBytesPerS != nil || first.BlockIO.ReadBytesPerS != nil {
		t.Fatalf("first sample must have null rates: %+v", first)
	}

	// 3 of 4 cores busy over the interval: 300 %.
	next := snap(t0.Add(2*time.Second), 4e9, 14e9)
	next.MemUsage, next.MemInactiveFile = 100<<20, 20<<20
	next.NetRx, next.NetTx = 1000, 4000
	next.BlkRead, next.BlkWrite = 2000, 6000
	next.Pids = 7
	rt.set(rt.containers, map[string]runtime.StatsSnapshot{id1: next})
	if err := s.Sample(ctx, t0.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	got := one(t, s, "")
	if !near(got.CPU, 300) {
		t.Fatalf("cpu = %v, want 300", got.CPU)
	}
	if got.Memory.Usage != 80<<20 || !near(got.Memory.Percent, 20) || got.Memory.Limit != 400<<20 {
		t.Fatalf("memory = %+v", got.Memory)
	}
	if !near(got.Network.RxBytesPerS, 500) || !near(got.Network.TxBytesPerS, 2000) ||
		!near(got.BlockIO.ReadBytesPerS, 1000) || !near(got.BlockIO.WriteBytesPerS, 3000) {
		t.Fatalf("rates = %+v %+v", got.Network, got.BlockIO)
	}
	if got.Pids != 7 || got.Name != "web-1" || got.Project != "app" || got.Service != "web" {
		t.Fatalf("identity = %+v", got)
	}
}

func TestContainerSamplerMemoryEdges(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	sn := snap(t0, 0, 0)
	sn.MemUsage, sn.MemInactiveFile, sn.MemLimit = 10, 50, 0 // cache larger than usage, no limit
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: sn})
	if err := s.Sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	got := one(t, s, "")
	if got.Memory.Usage != 0 || got.Memory.Percent != nil {
		t.Fatalf("memory = %+v, want usage 0, null percent", got.Memory)
	}
}

func TestContainerSamplerComposeServiceFallback(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	c := runtime.Container{ID: id1, Name: "x", State: "running", Labels: map[string]string{
		"com.docker.compose.service": "db", "ki.component": "edge"}}
	rt.set([]runtime.Container{c}, map[string]runtime.StatsSnapshot{id1: snap(t0, 0, 0)})
	if err := s.Sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if got := one(t, s, ""); got.Service != "db" || got.Component != "edge" {
		t.Fatalf("identity = %+v", got)
	}
}

func TestContainerSamplerSkipsStopped(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	stopped := running(id2, "app", "job")
	stopped.State = "exited"
	rt.set([]runtime.Container{running(id1, "app", "web"), stopped},
		map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1), id2: {}})
	if err := s.Sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if n := rt.statsCalls.Load(); n != 1 {
		t.Fatalf("Stats called %d times, want 1 (stopped container skipped)", n)
	}
	if cs := s.Current("", ""); len(cs) != 1 || cs[0].ID != id1 {
		t.Fatalf("current = %+v", cs)
	}
}

func TestContainerSamplerZeroReadIsNoSample(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: {}})
	if err := s.Sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if cs := s.Current("", ""); len(cs) != 0 {
		t.Fatalf("zero-read snapshot produced a sample: %+v", cs)
	}
}

func TestContainerSamplerReset(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	ctx := context.Background()
	at := func(sec int) time.Time { return t0.Add(time.Duration(sec) * time.Second) }
	feed := func(sec int, sn runtime.StatsSnapshot) ContainerSample {
		rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: sn})
		if err := s.Sample(ctx, at(sec*10)); err != nil {
			t.Fatal(err)
		}
		return one(t, s, "")
	}
	feed(0, snap(at(0), 5e9, 20e9))
	if got := feed(1, snap(at(10), 6e9, 24e9)); !near(got.CPU, 100) {
		t.Fatalf("cpu = %v, want 100", got.CPU)
	}
	// Container restarted: counters went backwards.
	restarted := snap(at(20), 1e8, 28e9)
	restarted.NetRx = 0
	if got := feed(2, restarted); got.CPU != nil || got.Network.RxBytesPerS != nil {
		t.Fatalf("after reset rates must be null: %+v", got)
	}
	// The new baseline is the post-reset snapshot.
	if got := feed(3, snap(at(30), 1e8+1e9, 32e9)); !near(got.CPU, 100) {
		t.Fatalf("cpu = %v, want 100 against the new baseline", got.CPU)
	}
}

func TestContainerSamplerNoSystemDelta(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	ctx := context.Background()
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 100)})
	_ = s.Sample(ctx, t0)
	rt.set(rt.containers, map[string]runtime.StatsSnapshot{id1: snap(t0.Add(time.Second), 2, 100)})
	_ = s.Sample(ctx, t0.Add(20*time.Second))
	if got := one(t, s, ""); got.CPU != nil {
		t.Fatalf("cpu = %v, want null when system delta is 0", *got.CPU)
	}
}

func TestContainerSamplerStaleSnapshotIgnored(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	ctx := context.Background()
	sn := snap(t0, 1, 1)
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: sn})
	_ = s.Sample(ctx, t0)
	_ = s.Sample(ctx, t0.Add(20*time.Second)) // engine returned the same Read
	hist := s.Query("", "", true)[0].History
	if len(*hist) != 1 {
		t.Fatalf("history = %d samples, want 1", len(*hist))
	}
}

func TestContainerSamplerGC(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	ctx := context.Background()
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1)})
	_ = s.Sample(ctx, t0)

	rt.set(nil, nil)
	_ = s.Sample(ctx, t0.Add(30*time.Minute))
	if cs := s.Current("", ""); len(cs) != 0 {
		t.Fatalf("vanished container still listed: %+v", cs)
	}
	// Within one history length the series is retained and addressable by id.
	if got := s.Query("", id1, true); len(got) != 1 || got[0].Current == nil {
		t.Fatalf("series dropped too early: %+v", got)
	}
	_ = s.Sample(ctx, t0.Add(61*time.Minute))
	if got := s.Query("", id1, true); len(got) != 0 {
		t.Fatalf("series not collected after one history length: %+v", got)
	}
}

func TestContainerSamplerRestartedContainerGetsNullRates(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	ctx := context.Background()
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1e9, 4e9)})
	_ = s.Sample(ctx, t0)
	rt.set(nil, nil)
	_ = s.Sample(ctx, t0.Add(20*time.Second))
	// Same id comes back with higher counters (baseline must have been dropped).
	rt.set([]runtime.Container{running(id1, "app", "web")},
		map[string]runtime.StatsSnapshot{id1: snap(t0.Add(40*time.Second), 9e9, 40e9)})
	_ = s.Sample(ctx, t0.Add(40*time.Second))
	if got := one(t, s, ""); got.CPU != nil {
		t.Fatalf("cpu = %v, want null after the container was away", *got.CPU)
	}
}

func TestContainerSamplerListError(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1)})
	_ = s.Sample(context.Background(), t0)
	if !s.Available() {
		t.Fatal("want available after a good sample")
	}
	gen := s.Gen()
	rt.setErr(runtime.ErrUnavailable)
	if err := s.Sample(context.Background(), t0.Add(time.Minute)); !errors.Is(err, runtime.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if s.Available() || s.Gen() == gen {
		t.Fatalf("available=%v gen advanced=%v", s.Available(), s.Gen() != gen)
	}
}

func TestContainerSamplerBoundedConcurrency(t *testing.T) {
	rt := &fakeRuntime{statsDelay: 20 * time.Millisecond}
	s := newSampler(rt)
	var cs []runtime.Container
	stats := map[string]runtime.StatsSnapshot{}
	for i := 0; i < 12; i++ {
		id := string(rune('a'+i)) + id1[1:]
		cs = append(cs, running(id, "app", "svc"))
		stats[id] = snap(t0, 1, 1)
	}
	rt.set(cs, stats)
	if err := s.Sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if mx := rt.maxFlight.Load(); mx > 4 || mx < 2 {
		t.Fatalf("max concurrent Stats = %d, want within 2..4", mx)
	}
	if got := len(s.Current("", "")); got != 12 {
		t.Fatalf("samples = %d, want 12", got)
	}
}

func TestContainerSamplerTimeout(t *testing.T) {
	rt := &fakeRuntime{statsDelay: time.Minute}
	s := NewContainerSampler(ContainerSamplerOptions{Runtime: rt, History: time.Hour, Resolution: time.Second, Timeout: 20 * time.Millisecond})
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1)})
	start := time.Now()
	if err := s.Sample(context.Background(), t0); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("hung Stats call was not cut off")
	}
	if cs := s.Current("", ""); len(cs) != 0 {
		t.Fatalf("timed-out container produced a sample: %+v", cs)
	}
}

func TestContainerSamplerFilters(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	rt.set([]runtime.Container{running(id1, "app", "web"), running(id2, "blog", "wp")},
		map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1), id2: snap(t0, 1, 1)})
	_ = s.Sample(context.Background(), t0)
	if cs := s.Current("", ""); len(cs) != 2 || cs[0].ID != id1 || cs[1].ID != id2 {
		t.Fatalf("all = %+v", cs)
	}
	if cs := s.Current("blog", ""); len(cs) != 1 || cs[0].ID != id2 {
		t.Fatalf("project filter = %+v", cs)
	}
	if cs := s.Current("", id1); len(cs) != 1 || cs[0].ID != id1 {
		t.Fatalf("id filter = %+v", cs)
	}
	if cs := s.Current("blog", id1); len(cs) != 0 {
		t.Fatalf("project+id mismatch = %+v", cs)
	}
}

func TestContainerSampleJSON(t *testing.T) {
	rt := &fakeRuntime{}
	s := newSampler(rt)
	rt.set([]runtime.Container{running(id1, "app", "web")}, map[string]runtime.StatsSnapshot{id1: snap(t0, 1, 1)})
	_ = s.Sample(context.Background(), t0)
	b, err := json.Marshal(s.Query("", "", false)[0])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["history"]; has {
		t.Fatalf("history present without being requested: %s", b)
	}
	cur := got["current"].(map[string]any)
	for _, k := range []string{"time", "id", "name", "project", "service", "component", "cpu_percent", "memory", "network", "block_io", "pids"} {
		if _, ok := cur[k]; !ok {
			t.Errorf("current lacks %q: %s", k, b)
		}
	}
	if v, ok := cur["cpu_percent"]; !ok || v != nil {
		t.Errorf("cpu_percent = %v, want null", v)
	}
	mem := cur["memory"].(map[string]any)
	for _, k := range []string{"usage", "limit", "percent"} {
		if _, ok := mem[k]; !ok {
			t.Errorf("memory lacks %q", k)
		}
	}
	if net := cur["network"].(map[string]any); net["rx_bytes_per_s"] != nil || net["tx_bytes_per_s"] != nil {
		t.Errorf("network = %v, want nulls", net)
	}
}
