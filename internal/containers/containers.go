// Package containers implements the containers.* and runtime.info JSON-RPC
// methods on top of a runtime.Runtime.
package containers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/runtime"
)

// Method and notification names.
const (
	MethodRuntimeInfo = "runtime.info"
	MethodList        = "containers.list"
	MethodInspect     = "containers.inspect"
	MethodStart       = "containers.start"
	MethodStop        = "containers.stop"
	MethodRestart     = "containers.restart"
	MethodRemove      = "containers.remove"
	MethodLogs        = "containers.logs.subscribe"
	MethodUnlogs      = "containers.logs.unsubscribe"
	NotifyLogs        = "containers.logs.update"
)

// Log tail bounds for containers.logs.subscribe.
const (
	DefaultTail = 100
	MaxTail     = 10000
)

// Handlers serves the container methods.
type Handlers struct {
	rt  runtime.Runtime
	log *slog.Logger
}

// New returns Handlers backed by rt. A nil logger uses slog.Default.
func New(rt runtime.Runtime, log *slog.Logger) *Handlers {
	if log == nil {
		log = slog.Default()
	}
	return &Handlers{rt: rt, log: log}
}

// Register installs the container methods on r. They sit behind whatever
// middleware r already has (the handshake gate).
func (h *Handlers) Register(r *rpc.Router) {
	r.Handle(MethodRuntimeInfo, h.runtimeInfo)
	r.Handle(MethodList, h.list)
	r.Handle(MethodInspect, h.inspect)
	r.Handle(MethodStart, h.start)
	r.Handle(MethodStop, h.stop)
	r.Handle(MethodRestart, h.restart)
	r.Handle(MethodRemove, h.remove)
	r.Handle(MethodLogs, h.logsSubscribe)
	r.Handle(MethodUnlogs, h.logsUnsubscribe)
}

// mapError translates runtime errors to JSON-RPC errors. Unknown and
// unmanaged containers are indistinguishable on purpose.
func mapError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrNotFound), errors.Is(err, runtime.ErrNotManaged):
		return rpc.NewError(rpc.CodeNotFound, "container not found")
	case errors.Is(err, runtime.ErrUnavailable):
		return rpc.NewError(rpc.CodeRuntimeUnavailable, "container runtime unavailable")
	}
	return err
}

type idParams struct {
	ID string `json:"id"`
}

func decodeID(params json.RawMessage, in any, id *string) error {
	if err := rpc.DecodeParams(params, in); err != nil {
		return err
	}
	if *id == "" {
		return rpc.NewError(rpc.CodeInvalidParams, "invalid params: id is required")
	}
	return nil
}

func (h *Handlers) runtimeInfo(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
	info, err := h.rt.Ping(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return info, nil
}

func (h *Handlers) list(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	cs, err := h.rt.List(ctx, in.Project)
	if err != nil {
		return nil, mapError(err)
	}
	if cs == nil {
		cs = []runtime.Container{}
	}
	return cs, nil
}

func (h *Handlers) inspect(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in idParams
	if err := decodeID(params, &in, &in.ID); err != nil {
		return nil, err
	}
	d, err := h.rt.Inspect(ctx, in.ID)
	if err != nil {
		return nil, mapError(err)
	}
	return d, nil
}

func (h *Handlers) start(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in idParams
	if err := decodeID(params, &in, &in.ID); err != nil {
		return nil, err
	}
	if err := h.rt.Start(ctx, in.ID); err != nil {
		return nil, mapError(err)
	}
	return struct{}{}, nil
}

type timeoutParams struct {
	ID       string `json:"id"`
	TimeoutS *int   `json:"timeout_s"`
}

// timeout returns the requested grace period; absent means the engine default
// (negative).
func (p timeoutParams) timeout() (time.Duration, error) {
	if p.TimeoutS == nil {
		return -1, nil
	}
	if *p.TimeoutS < 0 {
		return 0, rpc.NewError(rpc.CodeInvalidParams, "invalid params: timeout_s must not be negative")
	}
	return time.Duration(*p.TimeoutS) * time.Second, nil
}

func (h *Handlers) stop(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	return h.withTimeout(ctx, params, h.rt.Stop)
}

func (h *Handlers) restart(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	return h.withTimeout(ctx, params, h.rt.Restart)
}

func (h *Handlers) withTimeout(ctx context.Context, params json.RawMessage,
	op func(context.Context, string, time.Duration) error) (any, error) {
	var in timeoutParams
	if err := decodeID(params, &in, &in.ID); err != nil {
		return nil, err
	}
	d, err := in.timeout()
	if err != nil {
		return nil, err
	}
	if err := op(ctx, in.ID, d); err != nil {
		return nil, mapError(err)
	}
	return struct{}{}, nil
}

func (h *Handlers) remove(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		ID    string `json:"id"`
		Force bool   `json:"force"`
	}
	if err := decodeID(params, &in, &in.ID); err != nil {
		return nil, err
	}
	if err := h.rt.Remove(ctx, in.ID, in.Force); err != nil {
		return nil, mapError(err)
	}
	return struct{}{}, nil
}

// logLine is the data of a containers.logs.update notification carrying one
// log line.
type logLine struct {
	Stream string     `json:"stream"`
	Time   *time.Time `json:"time,omitempty"`
	Text   string     `json:"text"`
}

// logEnd is the final containers.logs.update data once the stream has ended.
type logEnd struct {
	EOF   bool   `json:"eof"`
	Error string `json:"error,omitempty"`
}

func (h *Handlers) logsSubscribe(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		ID         string  `json:"id"`
		Tail       *int    `json:"tail"`
		Since      *string `json:"since"`
		Timestamps bool    `json:"timestamps"`
	}
	if err := decodeID(params, &in, &in.ID); err != nil {
		return nil, err
	}
	opts := runtime.LogOptions{Follow: true, Tail: DefaultTail, Timestamps: in.Timestamps}
	if in.Tail != nil {
		if *in.Tail < 0 {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: tail must not be negative")
		}
		opts.Tail = min(*in.Tail, MaxTail)
	}
	if in.Since != nil && *in.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, *in.Since)
		if err != nil {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: since must be an RFC 3339 timestamp")
		}
		opts.Since = t
	}
	// Fail synchronously for unknown, unmanaged or unreachable.
	if _, err := h.rt.Inspect(ctx, in.ID); err != nil {
		return nil, mapError(err)
	}

	sub := p.Subs().Open(rpc.Ordered, NotifyLogs, 0)
	go h.streamLogs(sub, in.ID, opts)
	return map[string]any{"subscription": sub.ID()}, nil
}

// streamLogs feeds the subscription until the stream ends, the subscription is
// closed or its peer goes away. A natural end is announced with a final
// {eof: true} update, after which the subscription closes itself.
func (h *Handlers) streamLogs(sub *rpc.Subscription, id string, opts runtime.LogOptions) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-sub.Done():
			cancel()
		case <-ctx.Done():
		}
	}()

	err := h.rt.Logs(ctx, id, opts, func(l runtime.LogLine) error {
		u := logLine{Stream: l.Stream, Text: l.Text}
		if !l.Time.IsZero() {
			u.Time = &l.Time
		}
		return sub.Send(u)
	})
	select {
	case <-sub.Done():
		return // unsubscribed or peer gone: nobody to tell
	default:
	}
	eof := logEnd{EOF: true}
	if err != nil {
		h.log.Warn("container log stream failed", "container", id, "err", err)
		var re *rpc.Error
		if mapped := mapError(err); errors.As(mapped, &re) {
			eof.Error = re.Message
		} else {
			eof.Error = "log stream failed"
		}
	}
	_ = sub.Send(eof)
	sub.CloseAfterDrain()
}

func (h *Handlers) logsUnsubscribe(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
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
