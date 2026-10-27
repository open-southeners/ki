package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/store"
)

// mapErr converts service errors to RPC errors; unknown errors pass through
// (the router logs them and answers InternalError).
func mapErr(err error) error {
	var pe *paramError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe):
		return rpc.NewError(rpc.CodeInvalidParams, pe.msg)
	case errors.Is(err, store.ErrNotFound):
		return rpc.NewError(rpc.CodeNotFound, "not found")
	case errors.Is(err, store.ErrExists):
		return rpc.NewError(rpc.CodeAlreadyExists, "already exists")
	}
	return err
}

func call[P any](f func(ctx context.Context, p *rpc.Peer, in P) (any, error)) rpc.Handler {
	return func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
		var in P
		if err := rpc.DecodeParams(params, &in); err != nil {
			return nil, err
		}
		res, err := f(ctx, p, in)
		return res, mapErr(err)
	}
}

type none struct{}

type usernameParams struct {
	Username string `json:"username"`
}

// Register installs the account methods on r.
func (s *Service) Register(r *rpc.Router) {
	r.Handle("auth.whoami", call(func(_ context.Context, p *rpc.Peer, _ none) (any, error) {
		pr, ok := auth.PrincipalOf(p)
		if !ok {
			return nil, rpc.NewError(rpc.CodeUnauthorized, "unauthenticated")
		}
		return map[string]any{"kind": pr.Kind, "name": pr.Name, "role": pr.Role}, nil
	}))
	r.Handle("users.list", call(func(ctx context.Context, _ *rpc.Peer, _ none) (any, error) {
		return s.ListUsers(ctx)
	}))
	r.Handle("users.create", call(func(ctx context.Context, _ *rpc.Peer, in struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}) (any, error) {
		return s.CreateUser(ctx, in.Username, in.Role)
	}))
	r.Handle("users.update", call(func(ctx context.Context, _ *rpc.Peer, in struct {
		Username string  `json:"username"`
		Role     *string `json:"role"`
		Disabled *bool   `json:"disabled"`
	}) (any, error) {
		return s.UpdateUser(ctx, in.Username, in.Role, in.Disabled)
	}))
	r.Handle("users.reset", call(func(ctx context.Context, _ *rpc.Peer, in usernameParams) (any, error) {
		return s.ResetUser(ctx, in.Username)
	}))
	r.Handle("users.delete", call(func(ctx context.Context, _ *rpc.Peer, in usernameParams) (any, error) {
		if err := s.DeleteUser(ctx, in.Username); err != nil {
			return nil, err
		}
		return none{}, nil
	}))
	r.Handle("panel.setup", call(func(ctx context.Context, _ *rpc.Peer, in usernameParams) (any, error) {
		return s.PanelSetup(ctx, in.Username)
	}))
	r.Handle("tokens.create", call(func(ctx context.Context, p *rpc.Peer, in struct {
		Name      string `json:"name"`
		Role      string `json:"role"`
		ExpiresIn string `json:"expires_in"`
	}) (any, error) {
		var d time.Duration
		if in.ExpiresIn != "" {
			var err error
			if d, err = time.ParseDuration(in.ExpiresIn); err != nil {
				return nil, invalidf("expires_in must be a duration such as 720h")
			}
			if d <= 0 {
				return nil, invalidf("expires_in must be positive")
			}
		}
		pr, _ := auth.PrincipalOf(p)
		return s.CreateToken(ctx, pr.Name, in.Name, in.Role, d)
	}))
	r.Handle("tokens.list", call(func(ctx context.Context, _ *rpc.Peer, _ none) (any, error) {
		return s.ListTokens(ctx)
	}))
	r.Handle("tokens.revoke", call(func(ctx context.Context, _ *rpc.Peer, in struct {
		Name string `json:"name"`
	}) (any, error) {
		if err := s.RevokeToken(ctx, in.Name); err != nil {
			return nil, err
		}
		return none{}, nil
	}))
	r.Handle("audit.list", call(func(ctx context.Context, _ *rpc.Peer, in struct {
		Limit    int   `json:"limit"`
		BeforeID int64 `json:"before_id"`
	}) (any, error) {
		return s.ListAudit(ctx, in.Limit, in.BeforeID)
	}))
}
