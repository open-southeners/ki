package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-southeners/ki/api"
	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/rpc/rpctest"
	"github.com/open-southeners/ki/internal/transport/unixsock"
	"github.com/open-southeners/ki/internal/transport/ws"
)

// registerAuthProbes adds methods that expose the caller's identity and a
// viewer/admin pair for role checks.
func registerAuthProbes(r *rpc.Router) {
	r.Handle("test.whoami", func(_ context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		pr, _ := auth.PrincipalOf(p)
		return map[string]any{"principal": pr, "ip": auth.ClientIPOf(p)}, nil
	})
	r.Handle("test.admin", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return "ok", nil })
}

type whoami struct {
	Principal auth.Principal `json:"principal"`
	IP        string         `json:"ip"`
}

// dialHeader opens a WebSocket with hdr and completes agent.hello.
func dialHeader(t *testing.T, url string, hdr http.Header) (*client, int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := ws.DialWith(ctx, url, ws.DialOptions{Header: hdr})
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if err != nil {
		return nil, status, err
	}
	c := newClient(t, conn)
	if err := c.call(t, "agent.hello", map[string]any{"protocol": api.ProtocolVersion}, nil); err != nil {
		t.Fatal(err)
	}
	return c, status, nil
}

func bearer(tok string) http.Header { return http.Header{"Authorization": {"Bearer " + tok}} }
func cookie(id string) http.Header  { return http.Header{"Cookie": {SessionCookie + "=" + id}} }

func TestListenerPolicies(t *testing.T) {
	tests := []struct {
		name         string
		publicTokens bool
		noPanel      bool
		edge         bool
		hdr          http.Header
		want         bool
	}{
		{"direct token", false, false, false, bearer(testToken), true},
		{"direct cookie", false, false, false, cookie("sess"), true},
		{"direct bad token", false, false, false, bearer("nope"), false},
		{"direct bad cookie", false, false, false, cookie("nope"), false},
		{"direct anonymous", false, false, false, nil, false},
		{"direct token without panel", false, true, false, bearer(testToken), true},
		{"direct cookie without panel", false, true, false, cookie("sess"), false},
		{"edge cookie", false, false, true, cookie("sess"), true},
		{"edge token refused by default", false, false, true, bearer(testToken), false},
		{"edge token with public_tokens", true, false, true, bearer(testToken), true},
		{"edge cookie with public_tokens", true, false, true, cookie("sess"), true},
		{"edge cookie without panel", true, true, true, cookie("sess"), false},
		{"edge token without panel but public", true, true, true, bearer(testToken), true},
		{"edge refused token does not fall back to cookie", false, false, true,
			http.Header{"Authorization": {"Bearer " + testToken}, "Cookie": {SessionCookie + "=sess"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := startCfg(t, testOpts{edge: tt.edge, publicTokens: tt.publicTokens, noPanel: tt.noPanel})
			url := h.wsURL
			if tt.edge {
				url = h.edgeURL
			}
			_, status, err := dialHeader(t, url, tt.hdr)
			if tt.want {
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				return
			}
			if err == nil || status != http.StatusUnauthorized {
				t.Fatalf("want 401, got status %d err %v", status, err)
			}
		})
	}
}

// With the edge enabled the direct listener keeps accepting tokens while the
// edge one does not.
func TestBothListenersIndependent(t *testing.T) {
	h := startCfg(t, testOpts{edge: true})
	if _, _, err := dialHeader(t, h.wsURL, bearer(testToken)); err != nil {
		t.Fatalf("direct: %v", err)
	}
	if _, _, err := dialHeader(t, h.edgeURL, bearer(testToken)); err == nil {
		t.Fatal("edge accepted a token with public_tokens off")
	}
	if h.srv.APIAddr() == h.srv.EdgeAPIAddr() {
		t.Fatal("listeners share an address")
	}
}

func TestEdgeListenerOnlyWhenEnabled(t *testing.T) {
	h := start(t)
	if h.srv.EdgeAPIAddr() != "" {
		t.Fatalf("edge listener bound with the edge disabled: %s", h.srv.EdgeAPIAddr())
	}
}

func TestHTTPHandlerMountedOutsideRPC(t *testing.T) {
	panel := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("panel " + r.URL.Path)) })
	h := startCfg(t, testOpts{edge: true, http: panel})
	for _, base := range []string{h.srv.APIAddr(), h.srv.EdgeAPIAddr()} {
		resp, err := http.Get("http://" + base + "/auth/session")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", base, resp.StatusCode)
		}
		// /rpc is never handed to the panel handler.
		resp, err = http.Get("http://" + base + RPCPath)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: /rpc status = %d, want 401", base, resp.StatusCode)
		}
	}
}

func TestFromEdge(t *testing.T) {
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if FromEdge(r.Context()) {
			_, _ = w.Write([]byte("edge"))
			return
		}
		_, _ = w.Write([]byte("direct"))
	})
	if FromEdge(context.Background()) {
		t.Fatal("FromEdge true on a bare context")
	}
	h := startCfg(t, testOpts{edge: true, http: probe})
	for base, want := range map[string]string{h.srv.APIAddr(): "direct", h.srv.EdgeAPIAddr(): "edge"} {
		resp, err := http.Get("http://" + base + "/x")
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 16)
		n, _ := resp.Body.Read(b)
		resp.Body.Close()
		if got := string(b[:n]); got != want {
			t.Fatalf("%s: got %q, want %q", base, got, want)
		}
	}
}

func TestNilAuthRefusesRemote(t *testing.T) {
	h := startCfg(t, testOpts{noAuth: true, register: registerAuthProbes})
	if _, status, err := dialHeader(t, h.wsURL, bearer(testToken)); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("status %d err %v, want 401", status, err)
	}
	// The Unix socket does not depend on the authenticator.
	var got whoami
	if err := dialUnix(t, h).call(t, "test.whoami", nil, &got); err != nil {
		t.Fatal(err)
	}
}

func dialUnix(t *testing.T, h *harness) *client {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Helper()
	uc, err := unixsock.Dial(ctx, h.sock)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(t, uc)
	if err := c.call(t, "agent.hello", map[string]any{"protocol": api.ProtocolVersion}, nil); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestPrincipalsAndClientIP(t *testing.T) {
	h := startCfg(t, testOpts{edge: true, register: registerAuthProbes})

	var got whoami
	if err := dialUnix(t, h).call(t, "test.whoami", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Principal.Kind != auth.KindLocal || got.Principal.Role != auth.RoleAdmin || got.Principal.Name != "local" {
		t.Fatalf("unix principal = %+v", got.Principal)
	}

	// Direct listener: RemoteAddr, forwarded headers ignored.
	hdr := bearer(testToken)
	hdr.Set("X-Forwarded-For", "9.9.9.9")
	c, _, err := dialHeader(t, h.wsURL, hdr)
	if err != nil {
		t.Fatal(err)
	}
	got = whoami{}
	if err := c.call(t, "test.whoami", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Principal.Kind != auth.KindToken || got.Principal.ID != 1 || got.IP != "127.0.0.1" {
		t.Fatalf("direct = %+v", got)
	}

	// Edge listener: last X-Forwarded-For hop.
	hdr = cookie("sess")
	hdr.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	c, _, err = dialHeader(t, h.edgeURL, hdr)
	if err != nil {
		t.Fatal(err)
	}
	got = whoami{}
	if err := c.call(t, "test.whoami", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Principal.Kind != auth.KindUser || got.Principal.Name != "alice" || got.IP != "2.2.2.2" {
		t.Fatalf("edge = %+v", got)
	}
}

func TestServeConnIsControlPlane(t *testing.T) {
	h := startCfg(t, testOpts{register: registerAuthProbes})
	a, b := rpctest.Pipe()
	go h.srv.ServeConn(context.Background(), rpc.TransportControlPlane, a)
	c := newClient(t, b)
	if err := c.call(t, "agent.hello", map[string]any{"protocol": api.ProtocolVersion}, nil); err != nil {
		t.Fatal(err)
	}
	var got whoami
	if err := c.call(t, "test.whoami", nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.Principal.Kind != auth.KindControlPlane || got.Principal.Role != auth.RoleAdmin {
		t.Fatalf("principal = %+v", got.Principal)
	}
}

func TestViewerDeniedOnAdminMethod(t *testing.T) {
	a := newStubAuth()
	a.tokens["viewer-tok"] = auth.Principal{Kind: auth.KindToken, ID: 2, Name: "v", Role: auth.RoleViewer}
	h := startCfg(t, testOpts{register: registerAuthProbes, auth: a})

	v, _, err := dialHeader(t, h.wsURL, bearer("viewer-tok"))
	if err != nil {
		t.Fatal(err)
	}
	if err := v.call(t, "test.whoami", nil, nil); err != nil {
		t.Fatalf("viewer on viewer method: %v", err)
	}
	wantCode(t, v.call(t, "test.admin", nil, nil), rpc.CodeUnauthorized)

	adm, _, err := dialHeader(t, h.wsURL, bearer(testToken))
	if err != nil {
		t.Fatal(err)
	}
	if err := adm.call(t, "test.admin", nil, nil); err != nil {
		t.Fatalf("admin on admin method: %v", err)
	}
	if err := dialUnix(t, h).call(t, "test.admin", nil, nil); err != nil {
		t.Fatalf("local on admin method: %v", err)
	}
}

func TestUnlistedMethodDenied(t *testing.T) {
	h := startWith(t, func(r *rpc.Router) {
		r.Handle("test.unlisted", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) { return "ok", nil })
	})
	wantCode(t, dialUnix(t, h).call(t, "test.unlisted", nil, nil), rpc.CodeUnauthorized)
}

func waitClosed(t *testing.T, c *client, want bool) {
	t.Helper()
	select {
	case <-c.p.Done():
		if !want {
			t.Fatal("connection closed unexpectedly")
		}
	case <-time.After(map[bool]time.Duration{true: 2 * time.Second, false: 200 * time.Millisecond}[want]):
		if want {
			t.Fatal("connection not closed")
		}
	}
}

func TestCloseUserAndToken(t *testing.T) {
	h := startCfg(t, testOpts{})
	user, _, err := dialHeader(t, h.wsURL, cookie("sess"))
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := dialHeader(t, h.wsURL, bearer(testToken))
	if err != nil {
		t.Fatal(err)
	}

	h.srv.CloseUser(999) // unrelated id
	h.srv.CloseToken(7)  // a user's id used as a token id must not match
	waitClosed(t, user, false)
	waitClosed(t, tok, false)

	h.srv.CloseUser(7)
	waitClosed(t, user, true)
	waitClosed(t, tok, false)

	h.srv.CloseToken(1)
	waitClosed(t, tok, true)
}
