package monitor

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/runtime"
)

// Subscriber interval bounds: a subscription cannot make a sampler run faster
// than the lower bound or slower than the upper one.
const (
	minHostInterval      = time.Second
	minContainerInterval = 2 * time.Second
	maxInterval          = time.Minute
)

// Options configures a Manager.
type Options struct {
	Config  config.Monitor
	DataDir string
	// Runtime provides the container list and stats. A nil Runtime disables
	// container sampling (results report available: false).
	Runtime StatsRuntime
	// HostReader reads host counters; defaults to the gopsutil reader.
	HostReader HostReader
	Logger     *slog.Logger
	// Now returns the current time; defaults to time.Now.
	Now func() time.Time
}

// Manager samples the host and the managed containers on adaptive intervals,
// keeps their histories and serves them over RPC (see Register).
type Manager struct {
	cfg config.Monitor
	log *slog.Logger
	now func() time.Time

	host     *HostSampler
	hostRing *Ring[HostSample]
	hostSch  *schedule
	hostGen  atomic.Uint64
	hostNote *notifier
	// hostForce makes the host loop sample at once; hostPending counts
	// host.stats calls waiting for a per-core sample.
	hostForce   atomic.Bool
	hostPending atomic.Int64
	// contForce makes the container loop sample at once (see TriggerContainers).
	contForce atomic.Bool

	rt      StatsRuntime
	cont    *ContainerSampler
	contSch *schedule
	contNot *notifier

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// NewManager returns a Manager; call Start to begin sampling.
func NewManager(opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	reader := opts.HostReader
	if reader == nil {
		reader = NewGopsutilReader(log)
	}
	cfg := opts.Config
	paths := append([]string{opts.DataDir}, cfg.DiskPaths...)

	m := &Manager{
		cfg: cfg, log: log, now: now,
		host:     NewHostSampler(HostSamplerOptions{Reader: reader, DiskPaths: paths, Logger: log}),
		hostRing: NewRing[HostSample](cfg.History, cfg.HistoryResolution),
		hostSch:  newSchedule(cfg.HostInterval, minHostInterval, maxInterval),
		hostNote: newNotifier(),
		contSch:  newSchedule(cfg.ContainerInterval, minContainerInterval, maxInterval),
		contNot:  newNotifier(),
	}
	rt := opts.Runtime
	if rt == nil {
		rt = noRuntime{}
	}
	m.rt = rt
	m.cont = NewContainerSampler(ContainerSamplerOptions{
		Runtime: rt, History: cfg.History, Resolution: cfg.HistoryResolution, Logger: log,
	})
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m
}

// Start begins sampling in the background until ctx ends or Close is called.
// Only the first call has an effect.
func (m *Manager) Start(ctx context.Context) {
	m.once.Do(func() {
		stop := context.AfterFunc(ctx, m.cancel)
		m.wg.Add(2)
		go func() {
			defer m.wg.Done()
			defer stop()
			m.loop(m.hostSch, &m.hostForce, m.sampleHost)
		}()
		go func() {
			defer m.wg.Done()
			m.loop(m.contSch, &m.contForce, m.sampleContainers)
		}()
	})
}

// TriggerContainers makes the container loop sample at once instead of at its
// next tick, so freshly deployed containers show up quickly. It never blocks
// and coalesces bursts into a single sample.
func (m *Manager) TriggerContainers() {
	m.contForce.Store(true)
	m.contSch.poke()
}

// Close stops the sampling loops and waits for them. Subscriptions end with
// their peers.
func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

// loop runs sample whenever the schedule's interval has elapsed since the last
// run (immediately on start, when force is set, and when a subscriber change
// makes the next run due sooner).
func (m *Manager) loop(s *schedule, force *atomic.Bool, sample func(ctx context.Context, now time.Time)) {
	var last time.Time
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for m.ctx.Err() == nil {
		now := m.now()
		if last.IsZero() || (force != nil && force.Swap(false)) || !now.Before(last.Add(s.interval())) {
			sample(m.ctx, now)
			last = now
		}
		wait := last.Add(s.interval()).Sub(m.now())
		if wait < 0 {
			wait = 0
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(wait)
		select {
		case <-m.ctx.Done():
			return
		case <-timer.C:
		case <-s.wake:
		}
	}
}

func (m *Manager) sampleHost(ctx context.Context, now time.Time) {
	smp, err := m.host.Sample(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			m.log.Debug("host sample failed", "err", err)
		}
		return
	}
	m.hostRing.Add(now, smp)
	m.hostGen.Add(1)
	m.hostNote.fire()
}

func (m *Manager) sampleContainers(ctx context.Context, now time.Time) {
	if err := m.cont.Sample(ctx, now); err != nil && ctx.Err() == nil {
		m.log.Debug("container sample failed", "err", err)
	}
	m.contNot.fire()
}

// syncPerCore enables the per-core breakdown while any host subscriber or
// pending host.stats call wants it.
func (m *Manager) syncPerCore() {
	m.host.SetPerCore(m.hostSch.anyPerCore() || m.hostPending.Load() > 0)
}

// hostFor returns s as the caller asked for it: without the per-core
// breakdown unless perCore.
func hostFor(s HostSample, perCore bool) HostSample {
	if !perCore {
		return s.WithoutPerCore()
	}
	return s
}

// noRuntime stands in for a missing runtime: the engine is always down.
type noRuntime struct{}

func (noRuntime) List(context.Context, string) ([]runtime.Container, error) {
	return nil, runtime.ErrUnavailable
}

func (noRuntime) Stats(context.Context, string) (runtime.StatsSnapshot, error) {
	return runtime.StatsSnapshot{}, runtime.ErrUnavailable
}
