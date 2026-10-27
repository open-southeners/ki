package projects

import (
	"context"
	"encoding/json"

	"github.com/open-southeners/ki/internal/rpc"
)

func (h *Handlers) services(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var in slugParams
	if err := decodeSlug(params, &in, &in.Slug); err != nil {
		return nil, err
	}
	list, err := h.svc.Services(ctx, in.Slug)
	if err != nil {
		return nil, mapError(err)
	}
	return map[string][]ProjectService{"services": list}, nil
}

func (h *Handlers) serviceAction(fn func(ctx context.Context, slug, service string) (*ProjectService, error)) rpc.Handler {
	return func(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var in struct {
			Slug    string `json:"slug"`
			Service string `json:"service"`
		}
		if err := decodeSlug(params, &in, &in.Slug); err != nil {
			return nil, err
		}
		ps, err := fn(ctx, in.Slug, in.Service)
		if err != nil {
			return nil, mapError(err)
		}
		return ps, nil
	}
}
