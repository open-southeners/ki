package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Method names.
const (
	MethodCreate = "projects.create"
	MethodGet    = "projects.get"
	MethodList   = "projects.list"
	MethodUpdate = "projects.update"
	MethodVerify = "projects.verify"
	MethodDelete = "projects.delete"

	MethodExpose   = "projects.expose"
	MethodUnexpose = "projects.unexpose"

	MethodEnvSet   = "projects.env.set"
	MethodEnvUnset = "projects.env.unset"
	MethodEnvList  = "projects.env.list"
)

// Handlers serves the projects.* methods on top of a Service.
type Handlers struct {
	svc *Service
}

// NewHandlers returns Handlers backed by svc.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Register installs the project methods on r. They sit behind whatever
// middleware r already has (the handshake gate).
func (h *Handlers) Register(r *rpc.Router) {
	r.Handle(MethodCreate, h.create)
	r.Handle(MethodGet, h.get)
	r.Handle(MethodList, h.list)
	r.Handle(MethodUpdate, h.update)
	r.Handle(MethodVerify, h.verify)
	r.Handle(MethodDelete, h.delete)
	r.Handle(MethodExpose, h.expose)
	r.Handle(MethodUnexpose, h.unexpose)
	r.Handle(MethodEnvSet, h.envSet)
	r.Handle(MethodEnvUnset, h.envUnset)
	r.Handle(MethodEnvList, h.envList)
}

// mapError translates service errors to JSON-RPC errors.
func mapError(err error) error {
	var (
		inv *InvalidError
		ge  *git.Error
		dc  *store.DomainConflictError
	)
	switch {
	case err == nil:
		return nil
	case errors.As(err, &inv):
		return rpc.NewError(rpc.CodeInvalidParams, "invalid params: "+inv.Msg)
	case errors.As(err, &ge):
		msg := "repository error: " + ge.Reason
		if ge.Detail != "" {
			msg = "repository error: " + ge.Detail
		}
		return rpc.NewError(rpc.CodeRepositoryError, msg).
			WithData(map[string]string{"reason": ge.Reason, "detail": ge.Detail})
	case errors.As(err, &dc):
		return rpc.NewError(rpc.CodeAlreadyExists, fmt.Sprintf("domain %q is already exposed by project %q", dc.Domain, dc.Project)).
			WithData(map[string]string{"domain": dc.Domain, "project": dc.Project})
	case errors.Is(err, store.ErrNotFound):
		return rpc.NewError(rpc.CodeNotFound, "project not found")
	case errors.Is(err, store.ErrExists):
		return rpc.NewError(rpc.CodeAlreadyExists, "project already exists")
	case errors.Is(err, runtime.ErrUnavailable):
		return rpc.NewError(rpc.CodeRuntimeUnavailable, "container runtime unavailable")
	}
	return err
}

type slugParams struct {
	Slug string `json:"slug"`
}

func decodeSlug(params json.RawMessage, in any, slug *string) error {
	if err := rpc.DecodeParams(params, in); err != nil {
		return err
	}
	if *slug == "" {
		return rpc.NewError(rpc.CodeInvalidParams, "invalid params: slug is required")
	}
	return nil
}

func (h *Handlers) create(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in CreateParams
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.RepoURL == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: repo_url is required")
	}
	res, err := h.svc.Create(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return res, nil
}

func (h *Handlers) get(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in slugParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	p, err := h.svc.Get(ctx, in.Slug)
	if err != nil {
		return nil, mapError(err)
	}
	return p, nil
}

func (h *Handlers) list(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
	ps, err := h.svc.List(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return ps, nil
}

func (h *Handlers) update(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in UpdateParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	res, err := h.svc.Update(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return res, nil
}

func (h *Handlers) verify(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in slugParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	p, err := h.svc.Verify(ctx, in.Slug)
	if err != nil {
		return nil, mapError(err)
	}
	return p, nil
}

func (h *Handlers) delete(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug    string `json:"slug"`
		Volumes bool   `json:"volumes"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	rep, err := h.svc.Delete(ctx, in.Slug, in.Volumes)
	if err != nil {
		return nil, mapError(err)
	}
	return rep, nil
}

func (h *Handlers) envSet(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug string            `json:"slug"`
		Vars map[string]string `json:"vars"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	names, err := h.svc.SetEnv(ctx, in.Slug, in.Vars)
	if err != nil {
		return nil, mapError(err)
	}
	return map[string][]string{"names": names}, nil
}

func (h *Handlers) envUnset(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug  string   `json:"slug"`
		Names []string `json:"names"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	names, err := h.svc.UnsetEnv(ctx, in.Slug, in.Names)
	if err != nil {
		return nil, mapError(err)
	}
	return map[string][]string{"names": names}, nil
}

func (h *Handlers) envList(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in slugParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	vars, err := h.svc.ListEnv(ctx, in.Slug)
	if err != nil {
		return nil, mapError(err)
	}
	return map[string][]EnvEntry{"vars": vars}, nil
}

func (h *Handlers) expose(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in ExposeParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	p, err := h.svc.Expose(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return p, nil
}

func (h *Handlers) unexpose(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug    string `json:"slug"`
		Service string `json:"service"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	p, err := h.svc.Unexpose(ctx, in.Slug, in.Service)
	if err != nil {
		return nil, mapError(err)
	}
	return p, nil
}
