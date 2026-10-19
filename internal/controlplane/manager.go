package controlplane

import (
	"context"
	"errors"
	"log/slog"
	"sync"
)

// ErrManagerStopped is returned by Apply once Manager.Run has returned.
var ErrManagerStopped = errors.New("controlplane: manager stopped")

// Manager owns at most one Connector and can replace or disable it at runtime,
// so the control plane can be (re)configured without restarting the agent.
type Manager struct {
	log *slog.Logger

	// applyMu serializes Apply calls, which may block while an old connector
	// shuts down. mu guards the fields below and is never held while waiting.
	applyMu sync.Mutex
	mu      sync.Mutex
	runCtx  context.Context // nil until Run starts
	running bool
	stopped bool // Run has returned (or is returning)
	cur     *managed
	source  string
}

// managed is a Connector together with the means to stop it.
type managed struct {
	conn   *Connector
	cancel context.CancelFunc // nil until started
	done   chan struct{}      // closed when Run returned; nil until started
}

// NewManager returns a Manager with no connector (Status reports "disabled").
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{log: log}
}

// start runs mc under the manager's run context. Callers hold m.mu and have
// checked that Run is active.
func (m *Manager) start(mc *managed) {
	ctx, cancel := context.WithCancel(m.runCtx)
	mc.cancel = cancel
	mc.done = make(chan struct{})
	go func() {
		defer close(mc.done)
		_ = mc.conn.Run(ctx)
	}()
}

// stop cancels mc and waits for its Run to return. Cancelling the context
// handed to Serve ends the live WebSocket (the peer closes when it is done).
func stop(mc *managed) {
	if mc == nil || mc.cancel == nil {
		return
	}
	mc.cancel()
	<-mc.done
}

// Run starts the configured connector (if any) and serves Apply calls until
// ctx ends, then stops the connector, waits for it and returns nil. It returns
// an error if called more than once.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	if m.running || m.stopped {
		m.mu.Unlock()
		return errors.New("controlplane: manager already run")
	}
	m.running = true
	m.runCtx = ctx
	if m.cur != nil {
		m.start(m.cur)
	}
	m.mu.Unlock()

	<-ctx.Done()

	m.mu.Lock()
	m.stopped = true
	cur := m.cur
	m.mu.Unlock()
	stop(cur)
	return nil
}

// Apply replaces the active connector with one built from opts, tagged with
// source (reported in Status). A nil opts disables the control plane.
//
// opts is validated by New before anything is torn down, so an invalid
// configuration returns its error and leaves the running connection intact.
// When a connector is replaced, Apply waits for the old one to stop (closing
// its connection) before starting the new one. Before Run, the connector is
// only stored and Run starts it. After Run has returned, Apply changes nothing
// and returns ErrManagerStopped. Apply is safe for concurrent use; calls are
// serialized.
func (m *Manager) Apply(opts *Options, source string) error {
	var next *managed
	if opts != nil {
		o := *opts
		if o.Logger == nil {
			o.Logger = m.log
		}
		c, err := New(o)
		if err != nil {
			return err
		}
		next = &managed{conn: c}
	}

	m.applyMu.Lock()
	defer m.applyMu.Unlock()

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return ErrManagerStopped
	}
	old := m.cur
	m.mu.Unlock()

	stop(old)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return ErrManagerStopped
	}
	m.cur = next
	m.source = source
	if next != nil && m.running {
		m.start(next)
	}
	return nil
}

// Status returns the active connector's status with Source set, or a
// "disabled" status when there is none (or Run has returned).
func (m *Manager) Status() Status {
	m.mu.Lock()
	cur, source, stopped := m.cur, m.source, m.stopped
	m.mu.Unlock()
	if cur == nil || stopped {
		return Status{State: StateDisabled}
	}
	s := cur.conn.Status()
	s.Source = source
	return s
}

// StatusAny is Status as an any, for agent.Agent.SetControlPlane.
func (m *Manager) StatusAny() any { return m.Status() }
