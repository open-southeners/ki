package rpc

import (
	"context"
	"encoding/json"
)

// Transports a Peer can be served over, as recorded by SetTransport.
const (
	TransportUnix         = "unix"
	TransportWebSocket    = "websocket"
	TransportControlPlane = "control-plane"
)

// transportKey is the per-peer state key under which the transport is stored.
const transportKey = "rpc.transport"

// SetTransport records the transport p is served over. It must be called
// before p starts running.
func SetTransport(p *Peer, transport string) { p.Set(transportKey, transport) }

// TransportOf returns the transport recorded by SetTransport, or "" if none.
func TransportOf(p *Peer) string {
	v, _ := p.Get(transportKey)
	s, _ := v.(string)
	return s
}

// LocalOnly returns a Middleware that refuses the given methods with
// Unauthorized unless the peer is served over the Unix socket. A peer whose
// transport is unknown is refused too.
func LocalOnly(methods ...string) Middleware {
	set := make(map[string]struct{}, len(methods))
	for _, m := range methods {
		set[m] = struct{}{}
	}
	return func(method string, next Handler) Handler {
		if _, ok := set[method]; !ok {
			return next
		}
		return func(ctx context.Context, p *Peer, params json.RawMessage) (any, error) {
			if TransportOf(p) != TransportUnix {
				return nil, NewError(CodeUnauthorized, "only available on the local socket")
			}
			return next(ctx, p, params)
		}
	}
}
