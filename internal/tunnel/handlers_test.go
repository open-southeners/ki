package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
)

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

type stubCtl struct {
	status     Status
	configured []string
	removed    int
	err        error
}

func (s *stubCtl) Status() Status { return s.status }
func (s *stubCtl) Configure(token string) error {
	s.configured = append(s.configured, token)
	return s.err
}
func (s *stubCtl) Remove(context.Context) error { s.removed++; return s.err }

func hClient(t *testing.T, ctl Controller, transport string) *rpc.Peer {
	t.Helper()
	r := rpc.NewRouter()
	NewHandlers(ctl, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(r)
	ca, cb := rpctest.Pipe()
	srv := rpc.NewPeer(ca, r)
	rpc.SetTransport(srv, transport)
	go srv.Run(context.Background())
	cl := rpc.NewPeer(cb, rpc.NewRouter())
	go cl.Run(context.Background())
	t.Cleanup(func() { cl.Close(); srv.Close() })
	return cl
}

func call(t *testing.T, p *rpc.Peer, method string, params, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Call(ctx, method, params, out)
}

func wantCode(t *testing.T, err error, code int) *rpc.Error {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want code %d", err, code)
	}
	return re
}

func TestHandlersConfigureRemove(t *testing.T) {
	for _, tr := range []string{rpc.TransportUnix, rpc.TransportControlPlane} {
		ctl := &stubCtl{status: Status{Enabled: true, State: StateStarting, Configured: true, Warnings: []string{}}}
		c := hClient(t, ctl, tr)

		var st Status
		if err := call(t, c, MethodConfigure, map[string]string{"token": "  " + testToken + "\n"}, &st); err != nil {
			t.Fatalf("%s: %v", tr, err)
		}
		if len(ctl.configured) != 1 || ctl.configured[0] != testToken || st.State != StateStarting {
			t.Errorf("%s: configured = %v status = %+v", tr, ctl.configured, st)
		}
		js, _ := jsonOf(st)
		noToken(t, "configure result", js)

		if err := call(t, c, MethodRemove, nil, &st); err != nil || ctl.removed != 1 {
			t.Errorf("%s: remove: %v removed=%d", tr, err, ctl.removed)
		}
		if err := call(t, c, MethodStatus, nil, &st); err != nil {
			t.Errorf("%s: status: %v", tr, err)
		}
	}
}

func TestHandlersInvalidToken(t *testing.T) {
	ctl := &stubCtl{}
	c := hClient(t, ctl, rpc.TransportUnix)
	for _, tok := range []string{"", "   ", "no spaces allowed", strings.Repeat("A", 5000)} {
		err := call(t, c, MethodConfigure, map[string]string{"token": tok}, nil)
		re := wantCode(t, err, rpc.CodeInvalidParams)
		if tok != "" && strings.TrimSpace(tok) != "" && strings.Contains(re.Message, strings.TrimSpace(tok)) {
			t.Errorf("message echoes the token: %q", re.Message)
		}
	}
	wantCode(t, call(t, c, MethodConfigure, map[string]string{}, nil), rpc.CodeInvalidParams)
	if len(ctl.configured) != 0 {
		t.Errorf("invalid tokens reached the manager: %v", ctl.configured)
	}
}

func TestHandlersTransports(t *testing.T) {
	ctl := &stubCtl{}
	ws := hClient(t, ctl, rpc.TransportWebSocket)
	var st Status
	if err := call(t, ws, MethodStatus, nil, &st); err != nil {
		t.Fatalf("status over websocket: %v", err)
	}
	for _, m := range []string{MethodConfigure, MethodRemove} {
		re := wantCode(t, call(t, ws, m, map[string]string{"token": testToken}, nil), rpc.CodeUnauthorized)
		if re.Message != "only available on: unix, control-plane" {
			t.Errorf("%s message = %q", m, re.Message)
		}
	}
	if len(ctl.configured) != 0 || ctl.removed != 0 {
		t.Error("a refused call reached the manager")
	}
}

func TestHandlerStoreFailureHidesDetails(t *testing.T) {
	ctl := &stubCtl{err: errors.New("boom")}
	c := hClient(t, ctl, rpc.TransportUnix)
	err := call(t, c, MethodConfigure, map[string]string{"token": testToken}, nil)
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Errorf("error contains the token: %v", err)
	}
}
