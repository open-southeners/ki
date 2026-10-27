package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/compose"
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

	MethodComposeGet     = "projects.compose.get"
	MethodComposeSet     = "projects.compose.set"
	MethodComposeHistory = "projects.compose.history"

	MethodImageSet = "projects.image.set"

	MethodExpose   = "projects.expose"
	MethodUnexpose = "projects.unexpose"

	MethodServices       = "projects.services"
	MethodServiceStart   = "projects.services.start"
	MethodServiceStop    = "projects.services.stop"
	MethodServiceRestart = "projects.services.restart"

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
	r.Handle(MethodComposeGet, h.composeGet)
	r.Handle(MethodComposeSet, h.composeSet)
	r.Handle(MethodComposeHistory, h.composeHistory)
	r.Handle(MethodImageSet, h.imageSet)
	r.Handle(MethodExpose, h.expose)
	r.Handle(MethodUnexpose, h.unexpose)
	r.Handle(MethodServices, h.services)
	r.Handle(MethodServiceStart, h.serviceAction(h.svc.StartService))
	r.Handle(MethodServiceStop, h.serviceAction(h.svc.StopService))
	r.Handle(MethodServiceRestart, h.serviceAction(h.svc.RestartService))
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
		ve  *compose.ValidationError
	)
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ve):
		reason := "compose_invalid"
		if len(ve.Violations) > 0 {
			reason = "policy_violation"
		}
		data := map[string]any{"reason": reason, "detail": ve.Reason}
		if len(ve.Violations) > 0 {
			data["violations"] = ve.Violations
		}
		return rpc.NewError(rpc.CodeInvalidParams, "invalid params: compose file rejected: "+ve.Error()).WithData(data)
	case errors.Is(err, ErrVersionNotFound):
		return rpc.NewError(rpc.CodeNotFound, "compose version not found")
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
	case errors.Is(err, ErrServiceNotFound):
		return rpc.NewError(rpc.CodeNotFound, "service not found")
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

// principalName is the authenticated caller's name, "" when unknown.
func principalName(peer *rpc.Peer) string {
	if peer == nil {
		return ""
	}
	pr, _ := auth.PrincipalOf(peer)
	return pr.Name
}

func (h *Handlers) create(ctx context.Context, peer *rpc.Peer, params json.RawMessage) (any, error) {
	var in CreateParams
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	in.CreatedBy = principalName(peer)
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

func (h *Handlers) composeGet(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug    string `json:"slug"`
		Version int    `json:"version"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	v, err := h.svc.ComposeGet(ctx, in.Slug, in.Version)
	if err != nil {
		return nil, mapError(err)
	}
	return v, nil
}

func (h *Handlers) composeSet(ctx context.Context, peer *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug    string `json:"slug"`
		Compose string `json:"compose"`
		Note    string `json:"note"`
		Deploy  bool   `json:"deploy"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	if in.Compose == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: compose is required")
	}
	if peer != nil && rpc.TransportOf(peer) == rpc.TransportUnix {
		ctx = context.WithValue(ctx, viaCLIKey{}, true)
	}
	res, err := h.svc.ComposeSetAndDeploy(ctx, in.Slug, in.Compose, in.Note, principalName(peer), in.Deploy)
	if err != nil {
		return nil, mapError(err)
	}
	return res, nil
}

func (h *Handlers) composeHistory(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in struct {
		Slug  string `json:"slug"`
		Limit int    `json:"limit"`
	}
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	hs, err := h.svc.ComposeHistory(ctx, in.Slug, in.Limit)
	if err != nil {
		return nil, mapError(err)
	}
	return hs, nil
}

func (h *Handlers) imageSet(ctx context.Context, peer *rpc.Peer, params json.RawMessage) (any, error) {
	var in ImageSetParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	in.CreatedBy = principalName(peer)
	if peer != nil && rpc.TransportOf(peer) == rpc.TransportUnix {
		ctx = context.WithValue(ctx, viaCLIKey{}, true)
	}
	res, err := h.svc.ImageSet(ctx, in)
	if err != nil {
		return nil, mapError(err)
	}
	return res, nil
}
