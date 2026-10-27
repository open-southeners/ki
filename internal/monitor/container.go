package monitor

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-southeners/ki/internal/runtime"
)

// labelComponent and labelCompose are the labels naming a container's role
// (the edge) and, for plain compose containers, its service.
const (
	labelComponent = "ki.component"
	labelCompose   = "com.docker.compose.service"
)

// StatsRuntime is the part of runtime.Runtime the container sampler needs.
type StatsRuntime interface {
	List(ctx context.Context, project string) ([]runtime.Container, error)
	Stats(ctx context.Context, id string) (runtime.StatsSnapshot, error)
}

// ContainerSample is one container measurement. Rate fields are nil (JSON
// null) when unknown: on the first sample or after a counter reset.
type ContainerSample struct {
	Time      time.Time        `json:"time"`
	ID        string           `json:"id"`
	Name      string           `json:"name"`
	Project   string           `json:"project"`
	Service   string           `json:"service"`
	Component string           `json:"component"`
	CPU       *float64         `json:"cpu_percent"`
	Memory    ContainerMemory  `json:"memory"`
	Network   ContainerNetwork `json:"network"`
	BlockIO   ContainerBlockIO `json:"block_io"`
	Pids      uint64           `json:"pids"`
}

// ContainerMemory is the working-set memory (usage without reclaimable file
// cache, like `docker stats`) in bytes. Percent is nil without a limit.
type ContainerMemory struct {
	Usage   uint64   `json:"usage"`
	Limit   uint64   `json:"limit"`
	Percent *float64 `json:"percent"`
}

// ContainerNetwork is the traffic over all of the container's interfaces.
type ContainerNetwork struct {
	RxBytesPerS *float64 `json:"rx_bytes_per_s"`
	TxBytesPerS *float64 `json:"tx_bytes_per_s"`
}

// ContainerBlockIO is the block device throughput.
type ContainerBlockIO struct {
	ReadBytesPerS  *float64 `json:"read_bytes_per_s"`
	WriteBytesPerS *float64 `json:"write_bytes_per_s"`
}

// ContainerSeries is the query result for one container: its latest sample
// (nil if none yet) and, when requested, the retained history.
type ContainerSeries struct {
	Current *ContainerSample   `json:"current"`
	History *[]ContainerSample `json:"history,omitempty"`
}

// ContainerSamplerOptions configures a ContainerSampler.
type ContainerSamplerOptions struct {
	Runtime StatsRuntime
	// History and Resolution size the per-container rings.
	History    time.Duration
	Resolution time.Duration
	// Workers bounds concurrent Stats calls (default 4); Timeout bounds each
	// call (default 5s).
	Workers int
	Timeout time.Duration
	Logger  *slog.Logger
}

// ContainerSampler turns consecutive runtime stats snapshots into
// ContainerSamples and keeps a history per container. Sample is driven by one
// goroutine; the query methods are safe to call concurrently.
type ContainerSampler struct {
	rt      StatsRuntime
	history time.Duration
	res     time.Duration
	workers int
	timeout time.Duration
	log     *slog.Logger

	gen atomic.Uint64

	mu        sync.Mutex
	available bool
	series    map[string]*series
}

type series struct {
	info       ContainerSample // identity fields only
	prev       *runtime.StatsSnapshot
	ring       *Ring[ContainerSample]
	active     bool
	lastSample time.Time
}

// NewContainerSampler returns a sampler reading through opts.Runtime. It
// reports the engine as unavailable until the first successful Sample.
func NewContainerSampler(opts ContainerSamplerOptions) *ContainerSampler {
	s := &ContainerSampler{
		rt: opts.Runtime, history: opts.History, res: opts.Resolution,
		workers: opts.Workers, timeout: opts.Timeout, log: opts.Logger,
		series: make(map[string]*series),
	}
	if s.workers < 1 {
		s.workers = 4
	}
	if s.timeout <= 0 {
		s.timeout = 5 * time.Second
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s
}

// Gen returns a counter that advances after every Sample call.
func (s *ContainerSampler) Gen() uint64 { return s.gen.Load() }

// Available reports whether the last Sample could reach the engine.
func (s *ContainerSampler) Available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available
}

type fetched struct {
	snap runtime.StatsSnapshot
	ok   bool
}

// Sample lists the managed containers, reads the stats of the running ones
// and records a sample for each. A List failure marks the engine unavailable
// and is returned; per-container failures are skipped.
func (s *ContainerSampler) Sample(ctx context.Context, now time.Time) error {
	defer s.gen.Add(1)
	list, err := s.rt.List(ctx, "")
	if err != nil {
		s.mu.Lock()
		s.available = false
		s.mu.Unlock()
		return err
	}

	var running []runtime.Container
	for _, c := range list {
		if c.State == "running" {
			running = append(running, c)
		}
	}
	sort.Slice(running, func(i, j int) bool { return running[i].ID < running[j].ID })

	results := s.fetch(ctx, running)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.available = true
	for id, sr := range s.series {
		if !isRunning(running, id) {
			sr.active, sr.prev = false, nil
		}
	}
	for i, c := range running {
		sr := s.series[c.ID]
		if sr == nil {
			sr = &series{ring: NewRing[ContainerSample](s.history, s.res)}
			s.series[c.ID] = sr
		}
		sr.info = identity(c)
		sr.active = true
		if !results[i].ok {
			continue
		}
		if smp, ok := s.record(sr, results[i].snap, now); ok {
			sr.ring.Add(now, smp)
			sr.lastSample = now
		}
	}
	for id, sr := range s.series {
		if !sr.active && now.Sub(sr.lastSample) > s.history {
			delete(s.series, id)
		}
	}
	return nil
}

func isRunning(running []runtime.Container, id string) bool {
	i := sort.Search(len(running), func(i int) bool { return running[i].ID >= id })
	return i < len(running) && running[i].ID == id
}

func identity(c runtime.Container) ContainerSample {
	svc := c.Service
	if svc == "" {
		svc = c.Labels[labelCompose]
	}
	return ContainerSample{
		ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Project: c.Project,
		Service: svc, Component: c.Labels[labelComponent],
	}
}

// fetch reads the stats of cs with bounded concurrency; results keep the order
// of cs. A zero Read time (the engine's answer for a container that is not
// running) counts as no sample.
func (s *ContainerSampler) fetch(ctx context.Context, cs []runtime.Container) []fetched {
	out := make([]fetched, len(cs))
	sem := make(chan struct{}, s.workers)
	var wg sync.WaitGroup
	for i, c := range cs {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			cctx, cancel := context.WithTimeout(ctx, s.timeout)
			defer cancel()
			snap, err := s.rt.Stats(cctx, c.ID)
			if err != nil {
				if !errors.Is(err, runtime.ErrNotFound) && ctx.Err() == nil {
					s.log.Debug("container stats failed", "container", c.ID, "err", err)
				}
				return
			}
			out[i] = fetched{snap: snap, ok: !snap.Read.IsZero()}
		}()
	}
	wg.Wait()
	return out
}

// record derives a sample from snap and the series' previous snapshot, and
// makes snap the new baseline. It reports false if snap is not newer than the
// baseline (no new data).
func (s *ContainerSampler) record(sr *series, snap runtime.StatsSnapshot, now time.Time) (ContainerSample, bool) {
	smp := sr.info
	smp.Time = now.UTC()
	usage := snap.MemUsage
	if snap.MemInactiveFile >= usage {
		usage = 0
	} else {
		usage -= snap.MemInactiveFile
	}
	smp.Memory = ContainerMemory{Usage: usage, Limit: snap.MemLimit}
	if snap.MemLimit > 0 {
		p := float64(usage) / float64(snap.MemLimit) * 100
		smp.Memory.Percent = &p
	}
	smp.Pids = snap.Pids

	prev := sr.prev
	if prev != nil && !snap.Read.After(prev.Read) {
		return ContainerSample{}, false
	}
	sr.prev = &snap
	if prev == nil || counterReset(prev, &snap) {
		return smp, true
	}
	if dSys := float64(snap.SystemCPUNs - prev.SystemCPUNs); dSys > 0 && snap.OnlineCPUs > 0 {
		p := float64(snap.CPUTotalNs-prev.CPUTotalNs) / dSys * float64(snap.OnlineCPUs) * 100
		smp.CPU = &p
	}
	dt := snap.Read.Sub(prev.Read).Seconds()
	per := func(cur, old uint64) *float64 { r := float64(cur-old) / dt; return &r }
	smp.Network = ContainerNetwork{RxBytesPerS: per(snap.NetRx, prev.NetRx), TxBytesPerS: per(snap.NetTx, prev.NetTx)}
	smp.BlockIO = ContainerBlockIO{ReadBytesPerS: per(snap.BlkRead, prev.BlkRead), WriteBytesPerS: per(snap.BlkWrite, prev.BlkWrite)}
	return smp, true
}

// counterReset reports whether any cumulative counter went backwards, which
// means the container (or its cgroup) restarted.
func counterReset(prev, cur *runtime.StatsSnapshot) bool {
	return cur.CPUTotalNs < prev.CPUTotalNs || cur.SystemCPUNs < prev.SystemCPUNs ||
		cur.NetRx < prev.NetRx || cur.NetTx < prev.NetTx ||
		cur.BlkRead < prev.BlkRead || cur.BlkWrite < prev.BlkWrite
}

// Query returns the series matching the filters, ordered by container id.
// project filters by project slug; id selects one container by its full id
// (a stopped container with retained history is included, with its last
// sample). Without id only running containers with a sample are returned.
func (s *ContainerSampler) Query(project, id string, history bool) []ContainerSeries {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.series))
	for k := range s.series {
		ids = append(ids, k)
	}
	sort.Strings(ids)
	out := []ContainerSeries{}
	for _, k := range ids {
		sr := s.series[k]
		if id != "" && k != id {
			continue
		}
		if project != "" && sr.info.Project != project {
			continue
		}
		cur, ok := sr.ring.Latest()
		if id == "" && (!sr.active || !ok) {
			continue
		}
		cs := ContainerSeries{}
		if ok {
			cs.Current = &cur
		}
		if history {
			h := sr.ring.Snapshot()
			cs.History = &h
		}
		out = append(out, cs)
	}
	return out
}

// Current returns the latest sample of every container matching the filters
// (see Query), ordered by container id.
func (s *ContainerSampler) Current(project, id string) []ContainerSample {
	out := []ContainerSample{}
	for _, cs := range s.Query(project, id, false) {
		if cs.Current != nil {
			out = append(out, *cs.Current)
		}
	}
	return out
}
