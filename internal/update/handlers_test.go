package update

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/rpc/rpctest"
)

type hdlSource struct {
	st      UpdateStatus
	checked int
}

func (s *hdlSource) Status() UpdateStatus { return s.st }
func (s *hdlSource) CheckNow(context.Context) UpdateStatus {
	s.checked++
	return s.st
}

type hdlAudit struct {
	mu     sync.Mutex
	events []string
}

func (a *hdlAudit) add(_ context.Context, event, target, result string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, event+" "+target+" "+result)
}

func (a *hdlAudit) list() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.events...)
}

type hdlEnv struct {
	h     *Handlers
	dir   string
	src   *hdlSource
	audit *hdlAudit
	nowMu sync.Mutex
	now   time.Time
}

func newHdlEnv(t *testing.T, supported bool) *hdlEnv {
	t.Helper()
	e := &hdlEnv{dir: t.TempDir(), audit: &hdlAudit{}, now: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	e.src = &hdlSource{st: UpdateStatus{
		Current: "v0.3.0", Channel: ChannelStable, CheckEnabled: true, State: StateIdle,
		Latest: &Release{Tag: "v0.4.0", Name: "0.4.0"}, UpdateAvailable: true,
	}}
	e.h = NewHandlers(HandlerOptions{
		Source: e.src, Dir: e.dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:          e.audit.add,
		ApplySupported: func() bool { return supported },
		Now: func() time.Time {
			e.nowMu.Lock()
			defer e.nowMu.Unlock()
			return e.now
		},
		PollInterval:   5 * time.Millisecond,
		RequestTimeout: time.Minute,
	})
	return e
}

func (e *hdlEnv) client(t *testing.T, transport string, pr *auth.Principal) *rpc.Peer {
	t.Helper()
	r := rpc.NewRouter()
	e.h.Register(r)
	ca, cb := rpctest.Pipe()
	srv := rpc.NewPeer(ca, r)
	rpc.SetTransport(srv, transport)
	if pr != nil {
		auth.SetPrincipal(srv, *pr)
	}
	go srv.Run(context.Background())
	cl := rpc.NewPeer(cb, rpc.NewRouter())
	go cl.Run(context.Background())
	t.Cleanup(func() { cl.Close(); srv.Close() })
	return cl
}

func hdlCall(t *testing.T, p *rpc.Peer, method string, params, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Call(ctx, method, params, out)
}

func hdlWantCode(t *testing.T, err error, code int) *rpc.Error {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want code %d", err, code)
	}
	return re
}

var panelAdmin = &auth.Principal{Kind: auth.KindUser, ID: 1, Name: "ana", Role: auth.RoleAdmin}

func TestHandlersStatusAndCheck(t *testing.T) {
	e := newHdlEnv(t, true)
	c := e.client(t, rpc.TransportUnix, &auth.Principal{Kind: auth.KindLocal, Name: "root", Role: auth.RoleAdmin})
	var st UpdateStatus
	if err := hdlCall(t, c, MethodStatus, nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Current != "v0.3.0" || !st.UpdateAvailable || !st.ApplySupported || st.State != StateIdle || st.LastResult != nil {
		t.Errorf("status = %+v", st)
	}
	if err := hdlCall(t, c, MethodCheck, nil, &st); err != nil || e.src.checked != 1 {
		t.Errorf("check: %v checked=%d", err, e.src.checked)
	}
}

func TestHandlersApplyRefusedOutsidePanel(t *testing.T) {
	e := newHdlEnv(t, true)
	cases := map[string]struct {
		transport string
		pr        *auth.Principal
	}{
		"socket":        {rpc.TransportUnix, &auth.Principal{Kind: auth.KindLocal, Name: "root", Role: auth.RoleAdmin}},
		"token":         {rpc.TransportWebSocket, &auth.Principal{Kind: auth.KindToken, Name: "ci", Role: auth.RoleAdmin}},
		"control plane": {rpc.TransportControlPlane, &auth.Principal{Kind: auth.KindControlPlane, Name: "cp", Role: auth.RoleAdmin}},
		"user on unix":  {rpc.TransportUnix, panelAdmin},
		"no principal":  {rpc.TransportWebSocket, nil},
	}
	for name, tc := range cases {
		c := e.client(t, tc.transport, tc.pr)
		re := hdlWantCode(t, hdlCall(t, c, MethodApply, map[string]string{"version": "v0.4.0"}, nil), rpc.CodeUnauthorized)
		if re.Message != "updates are installed from the panel or with sudo ki agent update" {
			t.Errorf("%s: message = %q", name, re.Message)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, RequestFile)); err == nil {
		t.Error("a request was written")
	}
}

func TestHandlersApplyValidation(t *testing.T) {
	e := newHdlEnv(t, true)
	c := e.client(t, rpc.TransportWebSocket, panelAdmin)
	for name, v := range map[string]string{"malformed": "latest", "not latest": "v0.5.0", "empty": ""} {
		hdlWantCode(t, hdlCall(t, c, MethodApply, map[string]string{"version": v}, nil), rpc.CodeInvalidParams)
		_ = name
	}
	e.src.st.UpdateAvailable = false
	hdlWantCode(t, hdlCall(t, c, MethodApply, map[string]string{"version": "v0.4.0"}, nil), rpc.CodeInvalidParams)

	e = newHdlEnv(t, false)
	c = e.client(t, rpc.TransportWebSocket, panelAdmin)
	hdlWantCode(t, hdlCall(t, c, MethodApply, map[string]string{"version": "v0.4.0"}, nil), rpc.CodeInvalidParams)
	if _, err := os.Stat(filepath.Join(e.dir, RequestFile)); err == nil {
		t.Error("a request was written")
	}
}

func TestHandlersApplyWritesRequestAndFollowsResult(t *testing.T) {
	e := newHdlEnv(t, true)
	c := e.client(t, rpc.TransportWebSocket, panelAdmin)

	var out struct{ ID, State string }
	if err := hdlCall(t, c, MethodApply, map[string]string{"version": "v0.4.0"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID == "" || out.State != StateRequested {
		t.Fatalf("apply result = %+v", out)
	}
	// A second request while one is outstanding is refused.
	hdlWantCode(t, hdlCall(t, c, MethodApply, map[string]string{"version": "v0.4.0"}, nil), rpc.CodeInvalidParams)

	req, err := ReadRequest(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	if req.ID != out.ID || req.Version != "v0.4.0" || req.RequestedBy != "ana" {
		t.Errorf("request = %+v", req)
	}
	var st UpdateStatus
	if err := hdlCall(t, c, MethodStatus, nil, &st); err != nil || st.State != StateRequested {
		t.Fatalf("status = %+v, %v", st, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.h.Run(ctx)
	res := Result{ID: out.ID, Status: ResultSucceeded, From: "v0.3.0", To: "v0.4.0"}
	if err := WriteResult(e.dir, res, -1, -1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		return hdlCall(t, c, MethodStatus, nil, &st) == nil && st.State == StateIdle
	})
	if st.LastResult == nil || st.LastResult.Status != ResultSucceeded || st.LastResult.ID != out.ID {
		t.Errorf("last_result = %+v", st.LastResult)
	}
	waitFor(t, func() bool { return len(e.audit.list()) == 1 })
	if got := e.audit.list()[0]; got != "updates.result v0.4.0 succeeded" {
		t.Errorf("audit = %q", got)
	}
}

func TestHandlersRequestTimeout(t *testing.T) {
	e := newHdlEnv(t, true)
	c := e.client(t, rpc.TransportWebSocket, panelAdmin)
	if err := hdlCall(t, c, MethodApply, map[string]string{"version": "v0.4.0"}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.h.Run(ctx)

	// Before the deadline nothing changes.
	time.Sleep(30 * time.Millisecond)
	var st UpdateStatus
	if err := hdlCall(t, c, MethodStatus, nil, &st); err != nil || st.State != StateRequested {
		t.Fatalf("status = %+v, %v", st, err)
	}
	e.nowMu.Lock()
	e.now = e.now.Add(11 * time.Minute)
	e.nowMu.Unlock()
	waitFor(t, func() bool {
		return hdlCall(t, c, MethodStatus, nil, &st) == nil && st.State == StateIdle
	})
	if st.LastResult == nil || st.LastResult.Status != ResultFailed ||
		st.LastResult.Error != "the update helper did not run; is ki-agent-update.path enabled?" {
		t.Errorf("last_result = %+v", st.LastResult)
	}
}

func TestHandlersReportsExistingResultOnce(t *testing.T) {
	e := newHdlEnv(t, true)
	res := Result{ID: "abc", Status: ResultRolledBack, From: "v0.3.0", To: "v0.4.0", Error: "health check failed"}
	if err := WriteResult(e.dir, res, -1, -1); err != nil {
		t.Fatal(err)
	}
	run := func(h *Handlers) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { h.Run(ctx); close(done) }()
		time.Sleep(30 * time.Millisecond)
		cancel()
		<-done
	}
	run(e.h)
	if got := e.audit.list(); len(got) != 1 || got[0] != "updates.result v0.4.0 rolled_back" {
		t.Fatalf("audit after first start = %v", got)
	}
	var st UpdateStatus
	c := e.client(t, rpc.TransportUnix, &auth.Principal{Kind: auth.KindLocal, Name: "root", Role: auth.RoleAdmin})
	if err := hdlCall(t, c, MethodStatus, nil, &st); err != nil || st.LastResult == nil || st.LastResult.ID != "abc" {
		t.Fatalf("status = %+v, %v", st, err)
	}

	// A restart (new Handlers, same directory) does not report it again.
	h2 := NewHandlers(HandlerOptions{Source: e.src, Dir: e.dir, Audit: e.audit.add, PollInterval: 5 * time.Millisecond,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	run(h2)
	if got := e.audit.list(); len(got) != 1 {
		t.Errorf("audit after restart = %v", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
