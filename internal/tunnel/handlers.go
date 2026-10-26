package tunnel

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

// RPC method names.
const (
	MethodStatus    = "tunnel.status"
	MethodConfigure = "tunnel.configure"
	MethodRemove    = "tunnel.remove"
)

// Controller is the part of the [Manager] the handlers use.
type Controller interface {
	Status() Status
	Configure(token string) error
	Remove(ctx context.Context) error
}

// Handlers serves the tunnel.* methods.
type Handlers struct {
	mgr Controller
	log *slog.Logger
}

// NewHandlers returns handlers for the tunnel.
func NewHandlers(mgr Controller, logger *slog.Logger) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{mgr: mgr, log: logger}
}

// Register installs the tunnel methods on r. Changing the tunnel is limited to
// the local socket and the control plane (which may provision tunnels); the
// status is open to all.
func (h *Handlers) Register(r *rpc.Router) {
	r.Use(rpc.AllowTransports([]string{MethodConfigure, MethodRemove},
		rpc.TransportUnix, rpc.TransportControlPlane))
	r.Handle(MethodStatus, h.status)
	r.Handle(MethodConfigure, h.configure)
	r.Handle(MethodRemove, h.remove)
}

func (h *Handlers) status(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
	return h.mgr.Status(), nil
}

func (h *Handlers) configure(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var p struct {
		Token string `json:"token"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	token, err := CleanToken(p.Token)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: "+err.Error())
	}
	if err := h.mgr.Configure(token); err != nil {
		return nil, err
	}
	h.log.Info("tunnel configured")
	return h.mgr.Status(), nil
}

func (h *Handlers) remove(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
	if err := h.mgr.Remove(ctx); err != nil {
		return nil, err
	}
	h.log.Info("tunnel removed")
	return h.mgr.Status(), nil
}
