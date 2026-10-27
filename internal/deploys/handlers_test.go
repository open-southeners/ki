package deploys

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/rpc/rpctest"
	"github.com/open-southeners/ki/internal/webhooks"
)

type m = map[string]any

// client is an RPC client connected to a Handlers-backed server peer.
type client struct {
	p  *rpc.Peer
	mu sync.Mutex
	us []rpc.Update
}

func newClient(t *testing.T, mgr *Manager, transport string) *client {
	t.Helper()
	srv := rpc.NewRouter()
	NewHandlers(mgr).Register(srv)
	a, b := rpctest.Pipe()
	c := &client{}
	cr := rpc.NewRouter()
	cr.Handle(NotifyUpdate, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var u rpc.Update
		if err := json.Unmarshal(params, &u); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.us = append(c.us, u)
		c.mu.Unlock()
		return nil, nil
	})
	sp := rpc.NewPeer(a, srv)
	rpc.SetTransport(sp, transport)
	c.p = rpc.NewPeer(b, cr)
	go sp.Run(context.Background())
	go c.p.Run(context.Background())
	t.Cleanup(func() { c.p.Close(); sp.Close() })
	return c
}

func (c *client) call(method string, params, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.p.Call(ctx, method, params, result)
}

// capture is a subscription to a deploy's stream.
type capture struct {
	c   *client
	sub string
}

func newCapture(t *testing.T, mgr *Manager, id, since int64) *capture {
	t.Helper()
	c := newClient(t, mgr, rpc.TransportWebSocket)
	var res struct {
		Subscription string `json:"subscription"`
	}
	if err := c.call(MethodSubscribe, m{"id": id, "since_seq": since}, &res); err != nil || res.Subscription == "" {
		t.Fatalf("subscribe = %+v, %v", res, err)
	}
	return &capture{c: c, sub: res.Subscription}
}

// updates returns the decoded payloads in subscription order.
func (cp *capture) updates() []Update {
	cp.c.mu.Lock()
	us := append([]rpc.Update(nil), cp.c.us...)
	cp.c.mu.Unlock()
	sort.Slice(us, func(i, j int) bool { return us[i].Seq < us[j].Seq })
	var out []Update
	for _, u := range us {
		var p Update
		if json.Unmarshal(u.Data, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func (cp *capture) final() *Deploy {
	for _, u := range cp.updates() {
		if u.Type == "status" {
			return u.Deploy
		}
	}
	return nil
}

func (cp *capture) waitFinal(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cp.final() != nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no final status; updates = %+v", cp.updates())
}

func (cp *capture) has(stream, text string) bool {
	for _, u := range cp.updates() {
		if u.Type == "log" && u.Stream == stream && u.Text == text {
			return true
		}
	}
	return false
}

func (cp *capture) texts() []string {
	var out []string
	for _, u := range cp.updates() {
		if u.Type == "log" {
			out = append(out, u.Stream+":"+u.Text)
		}
	}
	return out
}

func wantCode(t *testing.T, err error, code int) *rpc.Error {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want rpc code %d", err, code)
	}
	return re
}

func TestSubscribeReplayThenLive(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	d := f.trigger(t, "")
	<-f.comp.entered

	// Joins mid-run: the stored lines first, then the live ones, no duplicates.
	cp := newCapture(t, f.mgr, d.ID, 0)
	stored, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
	if len(stored) < 3 {
		t.Fatalf("stored = %+v", stored)
	}
	// Resuming after n stored lines skips them.
	skip := newCapture(t, f.mgr, d.ID, int64(len(stored)))

	close(f.comp.block)
	cp.waitFinal(t)
	skip.waitFinal(t)
	f.wait(t, d.ID)

	all, _ := f.st.DeployLogs(context.Background(), d.ID, 0)
	var want []string
	for _, l := range all {
		want = append(want, l.Stream+":"+l.Text)
	}
	if got := cp.texts(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("stream =\n%s\nstored =\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := skip.texts(); strings.Join(got, "\n") != strings.Join(want[len(stored):], "\n") {
		t.Errorf("resumed stream = %v, want %v", got, want[len(stored):])
	}
	if fin := cp.final(); fin == nil || fin.ID != d.ID || fin.Status != StatusSucceeded {
		t.Errorf("final = %+v", fin)
	}
	// The status is the last update and the server closes the subscription.
	us := cp.updates()
	if us[len(us)-1].Type != "status" {
		t.Errorf("last update = %+v", us[len(us)-1])
	}
	wantCode(t, cp.c.call(MethodUnsubscribe, m{"subscription": cp.sub}, nil), rpc.CodeSubscriptionNotFound)

	// A finished deploy replays the stored log and the final status.
	done := newCapture(t, f.mgr, d.ID, 0)
	done.waitFinal(t)
	if got := done.texts(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("finished replay = %v", got)
	}
}

func TestSubscribeSupersededGetsFinalStatus(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.comp.block = make(chan struct{})
	f.comp.entered = make(chan string, 4)
	a := f.trigger(t, "")
	<-f.comp.entered
	b := f.trigger(t, "")
	cp := newCapture(t, f.mgr, b.ID, 0)
	f.trigger(t, "")
	cp.waitFinal(t)
	if fin := cp.final(); fin.Status != StatusSuperseded || fin.FinishedAt == nil {
		t.Errorf("final = %+v", fin)
	}
	close(f.comp.block)
	f.wait(t, a.ID)
}

func TestHandlers(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	local := newClient(t, f.mgr, rpc.TransportUnix)
	remote := newClient(t, f.mgr, rpc.TransportWebSocket)

	var d Deploy
	if err := local.call(MethodTrigger, m{"project": "app"}, &d); err != nil || d.Source != SourceCLI || d.ID == 0 {
		t.Fatalf("local trigger = %+v, %v", d, err)
	}
	f.wait(t, d.ID)
	var d2 Deploy
	if err := remote.call(MethodTrigger, m{"project": "app", "ref": "main"}, &d2); err != nil || d2.Source != SourceAPI {
		t.Fatalf("remote trigger = %+v, %v", d2, err)
	}
	f.wait(t, d2.ID)

	var got Deploy
	if err := remote.call(MethodGet, m{"id": d.ID}, &got); err != nil || got.ID != d.ID || got.Status != StatusSucceeded {
		t.Fatalf("get = %+v, %v", got, err)
	}
	var list []Deploy
	if err := remote.call(MethodList, m{"project": "app", "limit": 1}, &list); err != nil || len(list) != 1 || list[0].ID != d2.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := remote.call(MethodList, m{"project": "app", "before_id": d2.ID}, &list); err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Fatalf("list before = %+v, %v", list, err)
	}
	var empty json.RawMessage
	if err := remote.call(MethodList, m{"project": "app", "before_id": d.ID}, &empty); err != nil || string(empty) != "[]" {
		t.Errorf("empty list = %s, %v", empty, err)
	}

	// Synchronous failures.
	re := wantCode(t, remote.call(MethodTrigger, m{"project": "app", "ref": "dev"}, nil), rpc.CodeDeployError)
	if string(re.Data) != `{"detail":"only branch main can be deployed","reason":"ref_not_allowed"}` {
		t.Errorf("data = %s", re.Data)
	}
	re = wantCode(t, remote.call(MethodTrigger, m{"project": "app", "ref": "nope"}, nil), rpc.CodeDeployError)
	if !strings.Contains(string(re.Data), `"ref_not_found"`) {
		t.Errorf("data = %s", re.Data)
	}
	wantCode(t, remote.call(MethodTrigger, m{"project": "nope"}, nil), rpc.CodeNotFound)
	wantCode(t, remote.call(MethodTrigger, m{}, nil), rpc.CodeInvalidParams)
	wantCode(t, remote.call(MethodGet, m{"id": 9999}, nil), rpc.CodeNotFound)
	wantCode(t, remote.call(MethodGet, m{}, nil), rpc.CodeInvalidParams)
	wantCode(t, remote.call(MethodList, m{"project": "nope"}, nil), rpc.CodeNotFound)
	wantCode(t, remote.call(MethodList, m{"project": "app", "limit": 0}, nil), rpc.CodeInvalidParams)
	wantCode(t, remote.call(MethodSubscribe, m{"id": 9999}, nil), rpc.CodeNotFound)
	wantCode(t, remote.call(MethodSubscribe, m{"id": d.ID, "since_seq": -1}, nil), rpc.CodeInvalidParams)
	wantCode(t, remote.call(MethodUnsubscribe, m{}, nil), rpc.CodeInvalidParams)
	wantCode(t, remote.call(MethodUnsubscribe, m{"subscription": "x"}, nil), rpc.CodeSubscriptionNotFound)

	p, _ := f.st.GetProject(context.Background(), "app")
	p.Status = "pending_access"
	if err := f.st.UpdateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	re = wantCode(t, remote.call(MethodTrigger, m{"project": "app"}, nil), rpc.CodeDeployError)
	if !strings.Contains(string(re.Data), `"project_not_ready"`) {
		t.Errorf("data = %s", re.Data)
	}
}

func TestListLimitClamp(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	for range 3 {
		f.wait(t, f.trigger(t, "").ID)
	}
	if ds, _ := f.mgr.List(context.Background(), "app", 1000, 0); len(ds) != 3 {
		t.Errorf("len = %d", len(ds))
	}
	if MaxListLimit != 100 || DefaultListLimit != 20 {
		t.Error("list limits changed")
	}
}

func TestWebhookDeployer(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	var dep webhooks.Deployer = f.mgr.Deployer()
	id, err := dep.Trigger(context.Background(), webhooks.TriggerRequest{
		Project: "app", Ref: "refs/heads/main", Commit: f.repo.rev("main"), Source: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	got := f.wait(t, id)
	if got.Source != SourceWebhook || got.Status != StatusSucceeded {
		t.Errorf("deploy = %+v", got)
	}
	if _, err := dep.Trigger(context.Background(), webhooks.TriggerRequest{Project: "app", Ref: "refs/heads/dev"}); err == nil {
		t.Error("disallowed ref accepted")
	}
}
