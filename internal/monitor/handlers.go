package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/runtime"
)

// Method and notification names.
const (
	MethodHostStats             = "host.stats"
	MethodHostSubscribe         = "host.subscribe"
	MethodHostUnsubscribe       = "host.unsubscribe"
	NotifyHostUpdate            = "host.update"
	MethodContainersStats       = "containers.stats"
	MethodContainersSubscribe   = "containers.stats.subscribe"
	MethodContainersUnsubscribe = "containers.stats.unsubscribe"
	NotifyContainersUpdate      = "containers.stats.update"
)

// Default subscription intervals.
const (
	DefaultHostInterval      = 2 * time.Second
	DefaultContainerInterval = 5 * time.Second
)

const (
	// minIDPrefix is the shortest accepted container id prefix.
	minIDPrefix = 12
	// perCoreWait bounds how long host.stats waits for a per-core sample.
	perCoreWait = 2 * time.Second
)

// Register installs the monitoring methods on r.
func (m *Manager) Register(r *rpc.Router) {
	r.Handle(MethodHostStats, m.hostStats)
	r.Handle(MethodHostSubscribe, m.hostSubscribe)
	r.Handle(MethodHostUnsubscribe, unsubscribe)
	r.Handle(MethodContainersStats, m.containersStats)
	r.Handle(MethodContainersSubscribe, m.containersSubscribe)
	r.Handle(MethodContainersUnsubscribe, unsubscribe)
}

func (m *Manager) hostStats(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		History bool `json:"history"`
		PerCore bool `json:"per_core"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.PerCore {
		m.awaitPerCore(ctx)
	}
	out := struct {
		Current *HostSample   `json:"current"`
		History *[]HostSample `json:"history,omitempty"`
	}{}
	if cur, ok := m.hostRing.Latest(); ok {
		cur = hostFor(cur, in.PerCore)
		out.Current = &cur
	}
	if in.History {
		h := m.hostRing.Snapshot()
		for i := range h {
			h[i] = hostFor(h[i], in.PerCore)
		}
		out.History = &h
	}
	return out, nil
}

// awaitPerCore turns the per-core breakdown on and waits briefly for a sample
// that carries it, unless the latest one already does.
func (m *Manager) awaitPerCore(ctx context.Context) {
	if cur, ok := m.hostRing.Latest(); ok && len(cur.CPU.PerCore) > 0 {
		return
	}
	m.hostPending.Add(1)
	m.syncPerCore()
	defer func() {
		m.hostPending.Add(-1)
		m.syncPerCore()
	}()
	deadline := time.NewTimer(perCoreWait)
	defer deadline.Stop()
	for {
		ch := m.hostNote.wait()
		m.hostForce.Store(true)
		m.hostSch.poke()
		select {
		case <-ch:
		case <-deadline.C:
			return
		case <-ctx.Done():
			return
		case <-m.ctx.Done():
			return
		}
		if cur, ok := m.hostRing.Latest(); ok && len(cur.CPU.PerCore) > 0 {
			return
		}
	}
}

// intervalParam converts a client interval_ms into a Duration (before
// clamping). It rejects negative values.
func intervalParam(ms *int64, def time.Duration) (time.Duration, error) {
	if ms == nil {
		return def, nil
	}
	if *ms < 0 {
		return 0, rpc.NewError(rpc.CodeInvalidParams, "invalid params: interval_ms must not be negative")
	}
	return time.Duration(min(*ms, int64(maxInterval/time.Millisecond))) * time.Millisecond, nil
}

func (m *Manager) hostSubscribe(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		IntervalMS *int64 `json:"interval_ms"`
		PerCore    bool   `json:"per_core"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	req, err := intervalParam(in.IntervalMS, DefaultHostInterval)
	if err != nil {
		return nil, err
	}
	interval := m.hostSch.clamp(req)

	sub := p.Subs().Open(rpc.Snapshot, NotifyHostUpdate, interval)
	id := m.hostSch.add(interval, in.PerCore)
	m.syncPerCore()
	go m.hostUpdates(sub, id, interval, in.PerCore)
	return map[string]any{"subscription": sub.ID(), "interval_ms": interval.Milliseconds()}, nil
}

// hostUpdates sends the latest host sample every interval (and the first one
// as soon as it exists) until the subscription or the manager ends.
func (m *Manager) hostUpdates(sub *rpc.Subscription, id uint64, interval time.Duration, perCore bool) {
	defer func() {
		m.hostSch.remove(id)
		m.syncPerCore()
	}()
	t := time.NewTicker(interval)
	defer t.Stop()
	var sent uint64
	send := func() {
		gen := m.hostGen.Load()
		if gen == sent {
			return
		}
		cur, ok := m.hostRing.Latest()
		if !ok {
			return
		}
		sent = gen
		_ = sub.Send(map[string]any{"sample": hostFor(cur, perCore)})
	}
	for {
		ch := m.hostNote.wait()
		if sent == 0 {
			send()
		}
		select {
		case <-sub.Done():
			return
		case <-m.ctx.Done():
			return
		case <-t.C:
			send()
		case <-ch:
			// A sample arrived; the loop head delivers it if nothing was sent yet.
		}
	}
}

type containersResult struct {
	Available  bool              `json:"available"`
	Containers []ContainerSeries `json:"containers"`
}

type containerFilter struct {
	Project string `json:"project"`
	ID      string `json:"id"`
}

func (m *Manager) containersStats(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		containerFilter
		History bool `json:"history"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	id, down, err := m.resolveID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if down || !m.cont.Available() {
		return containersResult{Containers: []ContainerSeries{}}, nil
	}
	return containersResult{Available: true, Containers: m.cont.Query(in.Project, id, in.History)}, nil
}

// resolveID maps a full id or a prefix of at least 12 characters to the id of
// a managed container. An empty id resolves to itself. down reports that the
// engine could not be reached, in which case nothing is resolved and no error
// is returned.
func (m *Manager) resolveID(ctx context.Context, id string) (full string, down bool, err error) {
	if id == "" {
		return "", false, nil
	}
	notFound := rpc.NewError(rpc.CodeNotFound, "container not found")
	if len(id) < minIDPrefix {
		return "", false, notFound
	}
	list, err := m.rt.List(ctx, "")
	if err != nil {
		if errors.Is(err, runtime.ErrUnavailable) {
			return "", true, nil
		}
		return "", false, err
	}
	for _, c := range list {
		if c.ID == id {
			return c.ID, false, nil
		}
	}
	match := ""
	for _, c := range list {
		if strings.HasPrefix(c.ID, id) {
			if match != "" {
				return "", false, notFound // ambiguous
			}
			match = c.ID
		}
	}
	if match == "" {
		return "", false, notFound
	}
	return match, false, nil
}

func (m *Manager) containersSubscribe(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		containerFilter
		IntervalMS *int64 `json:"interval_ms"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	req, err := intervalParam(in.IntervalMS, DefaultContainerInterval)
	if err != nil {
		return nil, err
	}
	id, down, err := m.resolveID(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if down {
		return nil, rpc.NewError(rpc.CodeRuntimeUnavailable, "container runtime unavailable")
	}
	interval := m.contSch.clamp(req)

	sub := p.Subs().Open(rpc.Snapshot, NotifyContainersUpdate, interval)
	sid := m.contSch.add(interval, false)
	go m.containerUpdates(sub, sid, interval, in.Project, id)
	return map[string]any{"subscription": sub.ID(), "interval_ms": interval.Milliseconds()}, nil
}

// containerUpdates sends the latest container samples every interval (and the
// first update as soon as a sampling round has completed) until the
// subscription or the manager ends.
func (m *Manager) containerUpdates(sub *rpc.Subscription, sid uint64, interval time.Duration, project, id string) {
	defer m.contSch.remove(sid)
	t := time.NewTicker(interval)
	defer t.Stop()
	var sent uint64
	send := func() {
		gen := m.cont.Gen()
		if gen == sent {
			return
		}
		sent = gen
		avail := m.cont.Available()
		cs := []ContainerSample{}
		if avail {
			cs = m.cont.Current(project, id)
		}
		_ = sub.Send(map[string]any{"available": avail, "containers": cs})
	}
	for {
		ch := m.contNot.wait()
		if sent == 0 {
			send()
		}
		select {
		case <-sub.Done():
			return
		case <-m.ctx.Done():
			return
		case <-t.C:
			send()
		case <-ch:
			// A round finished; the loop head delivers it if nothing was sent yet.
		}
	}
}

func unsubscribe(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Subscription string `json:"subscription"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.Subscription == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: subscription is required")
	}
	if err := p.Subs().Close(in.Subscription); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}
