// Package agent implements the agent.* and rpc.* JSON-RPC methods.
package agent

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/version"
)

// Method and notification names.
const (
	MethodDiscover    = "rpc.discover"
	MethodHello       = "agent.hello"
	MethodInfo        = "agent.info"
	MethodPing        = "agent.ping"
	MethodHeartbeat   = "agent.heartbeat.subscribe"
	MethodUnheartbeat = "agent.heartbeat.unsubscribe"
	NotifyHeartbeat   = "agent.heartbeat.update"
)

// Heartbeat interval bounds.
const (
	DefaultHeartbeat = time.Second
	MinHeartbeat     = 250 * time.Millisecond
	MaxHeartbeat     = 60 * time.Second
)

// handshakeKey is the per-peer state flag set by a successful agent.hello.
const handshakeKey = "agent.handshaken"

// helloHookKey is the per-peer state key under which a hello hook is stored.
const helloHookKey = "agent.hello-hook"

// WithHelloHook returns a context carrying fn. A server that serves a
// connection with this context (see Server.ServeConn) arranges for fn to be
// called once each time agent.hello succeeds on that connection.
func WithHelloHook(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, helloHookKey, fn)
}

// HelloHook returns the hook stored in ctx by WithHelloHook, or nil.
func HelloHook(ctx context.Context) func() {
	fn, _ := ctx.Value(helloHookKey).(func())
	return fn
}

// SetHelloHook stores fn on p; agent.hello calls it after a successful
// handshake. It must be called before p starts running.
func SetHelloHook(p *rpc.Peer, fn func()) { p.Set(helloHookKey, fn) }

// capabilities the agent advertises in agent.hello.
var capabilities = []string{"heartbeat", "containers", "projects"}

// Agent holds the state shared by the method handlers.
type Agent struct {
	start time.Time

	controlPlane atomic.Pointer[func() any]
}

// disabledControlPlane is reported by agent.info until SetControlPlane is called.
var disabledControlPlane = map[string]string{"state": "disabled"}

// SetControlPlane sets the function agent.info calls for its control_plane
// field. fn must return a JSON-encodable value and be safe for concurrent use.
func (a *Agent) SetControlPlane(fn func() any) { a.controlPlane.Store(&fn) }

func (a *Agent) controlPlaneStatus() any {
	if fn := a.controlPlane.Load(); fn != nil {
		return (*fn)()
	}
	return disabledControlPlane
}

// New returns an Agent whose uptime starts now.
func New() *Agent { return &Agent{start: time.Now()} }

// Register installs the agent methods on r, and a middleware that makes every
// method except agent.hello and rpc.discover fail with HandshakeRequired until
// the peer has completed agent.hello.
func (a *Agent) Register(r *rpc.Router) {
	r.Use(requireHandshake)
	r.Handle(MethodDiscover, a.discover)
	r.Handle(MethodHello, a.hello)
	r.Handle(MethodInfo, a.info)
	r.Handle(MethodPing, a.ping)
	r.Handle(MethodHeartbeat, a.heartbeatSubscribe)
	r.Handle(MethodUnheartbeat, a.heartbeatUnsubscribe)
}

// Handshaken reports whether p has completed agent.hello. The server uses it
// to dispatch requests serially until then, so a client may pipeline requests
// right behind agent.hello.
func Handshaken(p *rpc.Peer) bool {
	v, _ := p.Get(handshakeKey)
	return v == true
}

func requireHandshake(method string, next rpc.Handler) rpc.Handler {
	if method == MethodHello || method == MethodDiscover {
		return next
	}
	return func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
		if !Handshaken(p) {
			return nil, rpc.NewError(rpc.CodeHandshakeRequired, "handshake required: call agent.hello first")
		}
		return next(ctx, p, params)
	}
}

func (a *Agent) discover(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
	return json.RawMessage(api.OpenRPC), nil
}

type helloParams struct {
	Protocol     string   `json:"protocol"`
	Client       any      `json:"client"`
	Capabilities []string `json:"capabilities"`
}

func (a *Agent) hello(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in helloParams
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	major, ok := parseMajor(in.Protocol)
	if !ok {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: protocol must be a semantic version such as 1.0.0")
	}
	want, _ := parseMajor(api.ProtocolVersion)
	if major != want {
		p.CloseAfterResponse()
		return nil, rpc.Errorf(rpc.CodeIncompatibleVersion,
			"incompatible protocol version %s: agent speaks %s", in.Protocol, api.ProtocolVersion).
			WithData(map[string]string{"supported": api.ProtocolVersion})
	}
	p.Set(handshakeKey, true)
	if fn, _ := p.Get(helloHookKey); fn != nil {
		if f, ok := fn.(func()); ok && f != nil {
			f()
		}
	}
	return map[string]any{
		"protocol": api.ProtocolVersion,
		"agent": map[string]string{
			"name":    "ki-agent",
			"version": version.Version,
			"commit":  version.Commit,
		},
		"capabilities": capabilities,
	}, nil
}

// parseMajor returns the major component of a MAJOR.MINOR.PATCH[-pre][+build]
// version.
func parseMajor(v string) (int, bool) {
	core, _, _ := strings.Cut(v, "+")
	core, _, _ = strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return 0, false
	}
	var major int
	for i, s := range parts {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, false
		}
		if i == 0 {
			major = n
		}
	}
	return major, true
}

func (a *Agent) info(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
	host, _ := os.Hostname()
	return map[string]any{
		"version":    version.Version,
		"commit":     version.Commit,
		"hostname":   host,
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"uptime_ms":  a.uptimeMS(),
		"go_version": runtime.Version(),

		"control_plane": a.controlPlaneStatus(),
	}, nil
}

func (a *Agent) uptimeMS() int64 { return time.Since(a.start).Milliseconds() }

func (a *Agent) ping(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
	return map[string]any{"pong": true, "time": time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

func (a *Agent) heartbeatSubscribe(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		IntervalMS *int64 `json:"interval_ms"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	interval := DefaultHeartbeat
	if in.IntervalMS != nil {
		if *in.IntervalMS < 0 {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: interval_ms must not be negative")
		}
		interval = time.Duration(min(*in.IntervalMS, int64(MaxHeartbeat/time.Millisecond))) * time.Millisecond
	}
	interval = rpc.ClampInterval(interval, MinHeartbeat, MaxHeartbeat)

	sub := p.Subs().Open(rpc.Snapshot, NotifyHeartbeat, interval)
	go a.heartbeat(sub, interval)
	return map[string]any{"subscription": sub.ID(), "interval_ms": interval.Milliseconds()}, nil
}

// heartbeat produces updates until the subscription or its peer ends.
func (a *Agent) heartbeat(sub *rpc.Subscription, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-sub.Done():
			return
		case now := <-t.C:
			_ = sub.Send(map[string]any{
				"time":      now.UTC().Format(time.RFC3339Nano),
				"uptime_ms": a.uptimeMS(),
			})
		}
	}
}

func (a *Agent) heartbeatUnsubscribe(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
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
