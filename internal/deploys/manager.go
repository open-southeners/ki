// Package deploys runs git deploys: it validates trigger requests, queues one
// deploy at a time per project, runs the fetch, normalize, policy-check, build,
// up and health pipeline, and streams and stores each deploy's log.
package deploys

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/projects"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/store"
	"github.com/open-southeners/ki-agent/internal/webhooks"
)

// Defaults for Options fields left zero.
const (
	DefaultHealthTimeout = 5 * time.Minute
	DefaultKeep          = 50
	DefaultLogMaxBytes   = 1 << 20

	defaultPollInterval = 2 * time.Second
	finalWriteTimeout   = 10 * time.Second
)

// Composer is the part of *compose.Runner the pipeline uses.
type Composer interface {
	Config(ctx context.Context, dir, composeFile, project string) ([]byte, error)
	Build(ctx context.Context, file, project string, out func(stream, line string)) error
	Up(ctx context.Context, file, project string, out func(stream, line string)) error
}

var _ Composer = (*compose.Runner)(nil)

// Runtime is the part of runtime.Runtime the health check uses.
type Runtime interface {
	List(ctx context.Context, project string) ([]runtime.Container, error)
	Inspect(ctx context.Context, id string) (runtime.ContainerDetail, error)
}

// Options configures a Manager.
type Options struct {
	Store    *store.Store
	Projects *projects.Service
	Git      *git.Client
	Compose  Composer
	Runtime  Runtime
	// DataDir holds deploys/<slug>/compose.json.
	DataDir string
	// HealthTimeout bounds the health stage (default DefaultHealthTimeout).
	HealthTimeout time.Duration
	// Keep is how many deploys per project are kept (default DefaultKeep).
	Keep int
	// LogMaxBytes caps the stored log of one deploy (default DefaultLogMaxBytes).
	LogMaxBytes int
	// PollInterval is the health check period (default 2s).
	PollInterval time.Duration
	Logger       *slog.Logger
}

// TriggerRequest asks for a deploy of Ref (empty: the trigger's default),
// optionally pinned to Commit.
type TriggerRequest struct {
	Project string
	Ref     string
	Commit  string
	// Source is webhook, api or cli (default api).
	Source string
}

// Manager queues and runs deploys.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	states map[string]*slugState
	casts  map[int64]*broadcaster
	wg     sync.WaitGroup
}

// slugState is the queue of one project; it exists while a worker runs.
type slugState struct {
	running, queued *job
	// deleting is set by Cancel: the project is going away, no new deploys.
	deleting bool
	// idle is closed when the worker has exited.
	idle chan struct{}
}

type job struct {
	d      *store.Deploy
	t      target
	ctx    context.Context
	cancel context.CancelFunc
	bc     *broadcaster
}

// New returns a Manager. Call Start before Trigger.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil || opts.Projects == nil || opts.Git == nil || opts.Compose == nil ||
		opts.Runtime == nil || opts.DataDir == "" {
		return nil, errors.New("deploys: store, projects, git, compose, runtime and data dir are required")
	}
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = DefaultHealthTimeout
	}
	if opts.Keep <= 0 {
		opts.Keep = DefaultKeep
	}
	if opts.LogMaxBytes <= 0 {
		opts.LogMaxBytes = DefaultLogMaxBytes
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPollInterval
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Manager{opts: opts, log: log, states: map[string]*slugState{}, casts: map[int64]*broadcaster{}}, nil
}

// Start marks deploys left unfinished by a previous process interrupted and
// readies the Manager; deploys run on contexts derived from ctx, so cancelling
// it interrupts them.
func (m *Manager) Start(ctx context.Context) error {
	n, err := m.opts.Store.InterruptUnfinishedDeploys(ctx)
	if err != nil {
		return fmt.Errorf("deploys: interrupt unfinished: %w", err)
	}
	if n > 0 {
		m.log.Warn("interrupted deploys left by a previous run", "count", n)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctx, m.cancel = context.WithCancel(ctx)
	return nil
}

// Close cancels every deploy (recording them interrupted) and waits for the
// workers to stop. It is idempotent.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.cancel == nil {
		m.closed = true
		m.mu.Unlock()
		return
	}
	m.closed = true
	m.cancel()
	var queued []*job
	for _, st := range m.states {
		if st.queued != nil {
			queued = append(queued, st.queued)
			st.queued = nil
		}
	}
	m.mu.Unlock()
	for _, j := range queued {
		m.finalize(j, StatusInterrupted, nil)
	}
	m.wg.Wait()
}

// Cancel cancels the running and the queued deploy of slug and waits until
// the project's worker has stopped (or ctx ends). It is meant for
// projects.Service.OnDelete.
func (m *Manager) Cancel(ctx context.Context, slug string) {
	m.mu.Lock()
	st := m.states[slug]
	if st == nil {
		m.mu.Unlock()
		return
	}
	q := st.queued
	st.queued = nil
	st.deleting = true
	if st.running != nil {
		st.running.cancel()
	}
	idle := st.idle
	m.mu.Unlock()
	if q != nil {
		q.cancel()
		m.finalize(q, StatusInterrupted, nil)
	}
	select {
	case <-idle:
	case <-ctx.Done():
	}
}

// Trigger validates the request, records a queued deploy and enqueues it. A
// deploy of a project with nothing running starts at once (status running).
// Validation failures are *TriggerError; an unknown project is
// store.ErrNotFound.
func (m *Manager) Trigger(ctx context.Context, req TriggerRequest) (*Deploy, error) {
	p, err := m.opts.Store.GetProject(ctx, req.Project)
	if err != nil {
		return nil, err
	}
	if p.Status == projects.StatusPending {
		return nil, &TriggerError{Reason: ReasonProjectNotReady, Detail: "the agent cannot read the repository yet"}
	}
	t, err := m.resolve(ctx, p, req.Ref, req.Commit)
	if err != nil {
		return nil, err
	}
	source := req.Source
	if source == "" {
		source = SourceAPI
	}
	d := &store.Deploy{Project: p.Slug, Ref: t.ref, Source: source, Status: StatusQueued}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx == nil {
		return nil, errors.New("deploys: not running")
	}
	if err := m.opts.Store.CreateDeploy(ctx, d); err != nil {
		return nil, err
	}
	jctx, cancel := context.WithCancel(m.ctx)
	j := &job{d: d, t: t, ctx: jctx, cancel: cancel,
		bc: newBroadcaster(m.opts.Store, m.log, d.ID, m.opts.LogMaxBytes)}
	m.casts[d.ID] = j.bc
	m.log.Info("deploy queued", "deploy", d.ID, "project", p.Slug, "ref", t.ref, "source", source)

	st := m.states[p.Slug]
	if st == nil {
		st = &slugState{idle: make(chan struct{})}
		m.states[p.Slug] = st
		m.begin(st, j)
		m.wg.Add(1)
		go m.worker(p.Slug, st)
	} else if st.deleting {
		cancel()
		delete(m.casts, d.ID)
		d.Status = StatusInterrupted
		now := time.Now().UTC()
		d.FinishedAt = &now
		m.save(j)
		return nil, errors.New("deploys: project is being deleted")
	} else {
		if old := st.queued; old != nil {
			old.cancel()
			m.finalizeLocked(old, StatusSuperseded, nil)
		}
		st.queued = j
	}
	v := View(d)
	return &v, nil
}

// begin makes j the running deploy of st. The caller holds m.mu.
func (m *Manager) begin(st *slugState, j *job) {
	now := time.Now().UTC()
	j.d.Status = StatusRunning
	j.d.StartedAt = &now
	st.running = j
	m.save(j)
}

func (m *Manager) worker(slug string, st *slugState) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		j := st.running
		m.mu.Unlock()
		m.execute(j)
		m.mu.Lock()
		next := st.queued
		st.queued = nil
		if next == nil {
			st.running = nil
			delete(m.states, slug)
			close(st.idle)
			m.mu.Unlock()
			return
		}
		m.begin(st, next)
		m.mu.Unlock()
	}
}

// finalize records j as finished with status (and the failure se, if any),
// pushes the final status to its subscribers and forgets its broadcaster.
func (m *Manager) finalize(j *job, status string, se *stepError) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finalizeLocked(j, status, se)
}

func (m *Manager) finalizeLocked(j *job, status string, se *stepError) {
	now := time.Now().UTC()
	j.d.Status = status
	j.d.FinishedAt = &now
	if se != nil {
		j.d.ErrorReason, j.d.ErrorDetail, j.d.ErrorData = se.reason, se.detail, se.data
	}
	m.save(j)
	j.bc.finish(View(j.d))
	delete(m.casts, j.d.ID)
	m.log.Info("deploy finished", "deploy", j.d.ID, "project", j.d.Project, "status", status,
		"reason", j.d.ErrorReason)
}

// save persists j's row; a deploy whose project is gone is not an error.
func (m *Manager) save(j *job) {
	ctx, cancel := context.WithTimeout(context.Background(), finalWriteTimeout)
	defer cancel()
	if err := m.opts.Store.UpdateDeploy(ctx, j.d); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.log.Error("save deploy", "deploy", j.d.ID, "err", err)
	}
}

// execute runs j to the end and records the outcome.
func (m *Manager) execute(j *job) {
	se := m.pipeline(j)
	status := StatusSucceeded
	switch {
	case se != nil && j.ctx.Err() != nil:
		status, se = StatusInterrupted, nil
		j.bc.line("agent", "deploy interrupted")
	case se != nil:
		status = StatusFailed
		j.bc.line("agent", fmt.Sprintf("deploy failed (%s): %s", se.reason, se.detail))
	default:
		j.bc.line("agent", "deploy succeeded")
	}
	m.finalize(j, status, se)
	j.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), finalWriteTimeout)
	defer cancel()
	if _, err := m.opts.Store.PruneDeploys(ctx, j.d.Project, m.opts.Keep); err != nil && !errors.Is(err, store.ErrNotFound) {
		m.log.Warn("prune deploys", "project", j.d.Project, "err", err)
	}
}

// Get returns the deploy with the given id (store.ErrNotFound if unknown).
func (m *Manager) Get(ctx context.Context, id int64) (*Deploy, error) {
	d, err := m.opts.Store.GetDeploy(ctx, id)
	if err != nil {
		return nil, err
	}
	v := View(d)
	return &v, nil
}

// Default and maximum page size of List.
const (
	DefaultListLimit = 20
	MaxListLimit     = 100
)

// List returns the project's deploys newest first. limit <= 0 selects
// DefaultListLimit and is capped at MaxListLimit; beforeID > 0 pages.
func (m *Manager) List(ctx context.Context, slug string, limit int, beforeID int64) ([]Deploy, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	ds, err := m.opts.Store.ListDeploys(ctx, slug, limit, beforeID)
	if err != nil {
		return nil, err
	}
	out := make([]Deploy, 0, len(ds))
	for _, d := range ds {
		out = append(out, View(d))
	}
	return out, nil
}

// Subscribe feeds sub the log of deploy id after sinceSeq (a stored line
// number, 1 based) and then its live log, ending with the final status after
// which sub is closed. A finished deploy replays the stored log and the final
// status at once. It returns store.ErrNotFound for an unknown deploy.
func (m *Manager) Subscribe(ctx context.Context, sub *rpc.Subscription, id, sinceSeq int64) error {
	d, err := m.opts.Store.GetDeploy(ctx, id)
	if err != nil {
		return err
	}
	m.mu.Lock()
	bc := m.casts[id]
	m.mu.Unlock()
	if bc != nil {
		return bc.attach(ctx, sub, sinceSeq)
	}
	// Finished: the broadcaster is dropped only after the final row is stored.
	if d, err = m.opts.Store.GetDeploy(ctx, id); err != nil {
		return err
	}
	if err := replayLog(ctx, m.opts.Store, sub, id, sinceSeq); err != nil {
		return err
	}
	v := View(d)
	if err := sub.Send(Update{Type: "status", Deploy: &v}); err != nil {
		return err
	}
	sub.CloseAfterDrain()
	return nil
}

// Deployer returns the adapter webhook receivers use to trigger deploys.
func (m *Manager) Deployer() webhooks.Deployer { return webhookDeployer{m} }

type webhookDeployer struct{ m *Manager }

func (w webhookDeployer) Trigger(ctx context.Context, req webhooks.TriggerRequest) (int64, error) {
	d, err := w.m.Trigger(ctx, TriggerRequest{Project: req.Project, Ref: req.Ref, Commit: req.Commit, Source: SourceWebhook})
	if err != nil {
		return 0, err
	}
	return d.ID, nil
}
