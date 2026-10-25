package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
)

// serveOn returns a client calling a router served over transport ("" = unset).
func serveOn(t *testing.T, r *rpc.Router, transport string) *running {
	t.Helper()
	ca, cb := rpctest.Pipe()
	server := rpc.NewPeer(ca, r)
	if transport != "" {
		rpc.SetTransport(server, transport)
	}
	go server.Run(context.Background())
	t.Cleanup(func() { server.Close() })
	return start(t, cb, rpc.NewRouter())
}

func TestAllowTransports(t *testing.T) {
	r := rpc.NewRouter()
	r.Use(rpc.AllowTransports([]string{"gated"}, rpc.TransportUnix, rpc.TransportControlPlane))
	h := func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return "ok", nil }
	r.Handle("gated", h)
	r.Handle("open", h)

	tests := []struct {
		transport, method string
		denied            bool
	}{
		{rpc.TransportUnix, "gated", false},
		{rpc.TransportControlPlane, "gated", false},
		{rpc.TransportWebSocket, "gated", true},
		{"", "gated", true},
		{rpc.TransportWebSocket, "open", false},
	}
	for _, tt := range tests {
		t.Run(tt.transport+"/"+tt.method, func(t *testing.T) {
			c := serveOn(t, r, tt.transport)
			var out string
			err := c.Call(ctxT(t), tt.method, nil, &out)
			if !tt.denied {
				if err != nil || out != "ok" {
					t.Fatalf("out=%q err=%v", out, err)
				}
				return
			}
			var re *rpc.Error
			if !errors.As(err, &re) || re.Code != rpc.CodeUnauthorized {
				t.Fatalf("err = %v", err)
			}
			if re.Message != "only available on: unix, control-plane" {
				t.Errorf("message = %q", re.Message)
			}
		})
	}
}
