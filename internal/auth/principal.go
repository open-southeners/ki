package auth

import (
	"context"
	"encoding/json"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

// Principal kinds.
const (
	KindUser         = "user"
	KindToken        = "token"
	KindLocal        = "local"
	KindControlPlane = "control-plane"
)

// Principal is the authenticated identity a Peer acts as.
type Principal struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
	Role Role   `json:"role"`
}

// principalKey is the per-peer state key under which the principal is stored.
const principalKey = "auth.principal"

// SetPrincipal records the principal p acts as. It must be called before p
// starts running.
func SetPrincipal(p *rpc.Peer, pr Principal) { p.Set(principalKey, pr) }

// PrincipalOf returns the principal recorded by SetPrincipal.
func PrincipalOf(p *rpc.Peer) (Principal, bool) {
	v, ok := p.Get(principalKey)
	if !ok {
		return Principal{}, false
	}
	pr, ok := v.(Principal)
	return pr, ok
}

// RequireRoles returns a Middleware enforcing table. A method absent from the
// table is denied as if it required admin; a peer without a principal is
// "unauthenticated"; a principal below the required role is "forbidden".
func RequireRoles(table map[string]Role) rpc.Middleware {
	return func(method string, next rpc.Handler) rpc.Handler {
		need, listed := table[method]
		if !listed {
			need = RoleAdmin
		}
		denied := rpc.NewError(rpc.CodeUnauthorized, "forbidden: requires "+need.String())
		return func(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
			if !listed {
				return nil, denied
			}
			pr, has := PrincipalOf(p)
			if !has {
				return nil, rpc.NewError(rpc.CodeUnauthorized, "unauthenticated")
			}
			if !pr.Role.AtLeast(need) {
				return nil, denied
			}
			return next(ctx, p, params)
		}
	}
}
