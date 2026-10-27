package deploys

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/open-southeners/ki/internal/git"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/runtime"
	"github.com/open-southeners/ki/internal/store"
)

// Method and notification names.
const (
	MethodTrigger     = "deploys.trigger"
	MethodRollback    = "deploys.rollback"
	MethodGet         = "deploys.get"
	MethodList        = "deploys.list"
	MethodSubscribe   = "deploys.subscribe"
	MethodUnsubscribe = "deploys.unsubscribe"
	// NotifyUpdate carries a deploy's log lines and final status.
	NotifyUpdate = "deploys.update"
)

// Handlers serves the deploys.* methods on top of a Manager.
type Handlers struct {
	m *Manager
}

// NewHandlers returns Handlers backed by m.
func NewHandlers(m *Manager) *Handlers { return &Handlers{m: m} }

// Register installs the deploy methods on r.
func (h *Handlers) Register(r *rpc.Router) {
	r.Handle(MethodTrigger, h.trigger)
	r.Handle(MethodRollback, h.rollback)
	r.Handle(MethodGet, h.get)
	r.Handle(MethodList, h.list)
	r.Handle(MethodSubscribe, h.subscribe)
	r.Handle(MethodUnsubscribe, h.unsubscribe)
}

// mapError translates manager errors to JSON-RPC errors.
func mapError(err error) error {
	var (
		te *TriggerError
		ge *git.Error
	)
	switch {
	case err == nil:
		return nil
	case errors.As(err, &te):
		msg := "deploy rejected: " + te.Reason
		if te.Detail != "" {
			msg = "deploy rejected: " + te.Detail
		}
		return rpc.NewError(rpc.CodeDeployError, msg).
			WithData(map[string]string{"reason": te.Reason, "detail": te.Detail})
	case errors.As(err, &ge):
		msg := "repository error: " + ge.Reason
		if ge.Detail != "" {
			msg = "repository error: " + ge.Detail
		}
		return rpc.NewError(rpc.CodeRepositoryError, msg).
			WithData(map[string]string{"reason": ge.Reason, "detail": ge.Detail})
	case errors.Is(err, runtime.ErrUnavailable):
		return rpc.NewError(rpc.CodeRuntimeUnavailable, "container runtime unavailable")
	}
	return err
}

func projectNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return rpc.NewError(rpc.CodeNotFound, "project not found")
	}
	return mapError(err)
}

func deployNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return rpc.NewError(rpc.CodeNotFound, "deploy not found")
	}
	return mapError(err)
}

func (h *Handlers) trigger(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
		Ref     string `json:"ref"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.Project == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: project is required")
	}
	source := SourceAPI
	if rpc.TransportOf(p) == rpc.TransportUnix {
		source = SourceCLI
	}
	d, err := h.m.Trigger(ctx, TriggerRequest{Project: in.Project, Ref: in.Ref, Source: source})
	if err != nil {
		return nil, projectNotFound(err)
	}
	return d, nil
}

func (h *Handlers) rollback(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Project string `json:"project"`
		Deploy  int64  `json:"deploy"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.Project == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: project is required")
	}
	if in.Deploy < 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: deploy must not be negative")
	}
	source := SourceAPI
	if rpc.TransportOf(p) == rpc.TransportUnix {
		source = SourceCLI
	}
	d, err := h.m.Rollback(ctx, RollbackRequest{Project: in.Project, Deploy: in.Deploy, Source: source})
	if err != nil {
		return nil, projectNotFound(err)
	}
	return d, nil
}

func (h *Handlers) get(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	id, err := decodeID(params)
	if err != nil {
		return nil, err
	}
	d, err := h.m.Get(ctx, id)
	if err != nil {
		return nil, deployNotFound(err)
	}
	return d, nil
}

func (h *Handlers) list(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Project  string `json:"project"`
		Limit    *int   `json:"limit"`
		BeforeID int64  `json:"before_id"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.Project == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: project is required")
	}
	limit := 0
	if in.Limit != nil {
		if *in.Limit < 1 {
			return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: limit must be positive")
		}
		limit = *in.Limit
	}
	if in.BeforeID < 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: before_id must not be negative")
	}
	ds, err := h.m.List(ctx, in.Project, limit, in.BeforeID)
	if err != nil {
		return nil, projectNotFound(err)
	}
	return ds, nil
}

func decodeID(params json.RawMessage) (int64, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return 0, err
	}
	if in.ID <= 0 {
		return 0, rpc.NewError(rpc.CodeInvalidParams, "invalid params: id is required")
	}
	return in.ID, nil
}

func (h *Handlers) subscribe(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		ID       int64 `json:"id"`
		SinceSeq int64 `json:"since_seq"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.ID <= 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: id is required")
	}
	if in.SinceSeq < 0 {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: since_seq must not be negative")
	}
	// Fail synchronously for an unknown deploy.
	if _, err := h.m.Get(ctx, in.ID); err != nil {
		return nil, deployNotFound(err)
	}
	sub := p.Subs().Open(rpc.Ordered, NotifyUpdate, 0)
	if err := h.m.Subscribe(ctx, sub, in.ID, in.SinceSeq); err != nil {
		sub.Close()
		return nil, deployNotFound(err)
	}
	return map[string]any{"subscription": sub.ID()}, nil
}

func (h *Handlers) unsubscribe(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
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

// MapError translates a Trigger or Rollback error to the JSON-RPC error the
// deploys methods answer with, for callers that trigger deploys on behalf of
// another method.
func MapError(err error) error { return projectNotFound(err) }
