package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/runtime"
)

// svcRuntime is a fake engine with a fixed set of containers.
type svcRuntime struct {
	fakeRuntime
	list    []runtime.Container
	detail  map[string]runtime.ContainerDetail
	actions []string
	failID  string
	failErr error
	onAct   func(id string)
}

func (r *svcRuntime) List(_ context.Context, project string) ([]runtime.Container, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.list, nil
}

func (r *svcRuntime) Inspect(_ context.Context, id string) (runtime.ContainerDetail, error) {
	d, ok := r.detail[id]
	if !ok {
		return runtime.ContainerDetail{}, runtime.ErrNotFound
	}
	return d, nil
}

func (r *svcRuntime) act(verb, id string, state string) error {
	r.actions = append(r.actions, verb+":"+id)
	if r.onAct != nil {
		r.onAct(id)
	}
	if id == r.failID {
		return r.failErr
	}
	d := r.detail[id]
	d.State = state
	r.detail[id] = d
	return nil
}

func (r *svcRuntime) Start(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.act("start", id, "running")
}
func (r *svcRuntime) Stop(ctx context.Context, id string, to time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if to != 10*time.Second {
		return fmt.Errorf("timeout = %v", to)
	}
	return r.act("stop", id, "exited")
}
func (r *svcRuntime) Restart(ctx context.Context, id string, to time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if to != 10*time.Second {
		return fmt.Errorf("timeout = %v", to)
	}
	return r.act("restart", id, "running")
}

// addC registers a container of service svc.
func (r *svcRuntime) addC(id, svc, state, health string) {
	if r.detail == nil {
		r.detail = map[string]runtime.ContainerDetail{}
	}
	c := runtime.Container{ID: id, Name: "app-" + svc + "-" + id, Image: "img-" + svc, State: state, Service: svc}
	r.list = append(r.list, c)
	r.detail[id] = runtime.ContainerDetail{Container: c, Health: health, RestartCount: 2,
		StartedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

func newSvcEnv(t testing.TB) (*env, *svcRuntime) {
	e := newEnv(t)
	rt := &svcRuntime{}
	e.svc.rt = rt
	e.seedProject(t, "app")
	return e, rt
}

func writeModel(t testing.TB, e *env, body string) {
	t.Helper()
	dir := filepath.Join(e.data, "deploys", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeriveServiceState(t *testing.T) {
	c := func(state, health string) ServiceContainer { return ServiceContainer{State: state, Health: health} }
	tests := []struct {
		name string
		cs   []ServiceContainer
		want string
	}{
		{"none", nil, ServiceMissing},
		{"running", []ServiceContainer{c("running", ""), c("running", "healthy")}, ServiceRunning},
		{"starting is running", []ServiceContainer{c("running", "starting")}, ServiceRunning},
		{"partial", []ServiceContainer{c("running", ""), c("exited", "")}, ServicePartial},
		{"stopped", []ServiceContainer{c("exited", ""), c("created", "")}, ServiceStopped},
		{"unhealthy", []ServiceContainer{c("running", "unhealthy"), c("running", "")}, ServiceUnhealthy},
		{"restarting", []ServiceContainer{c("restarting", "")}, ServiceUnhealthy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveServiceState(tt.cs); got != tt.want {
				t.Errorf("state = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestServicesModelAndExtras(t *testing.T) {
	ctx := context.Background()
	e, rt := newSvcEnv(t)
	writeModel(t, e, `{"services":{"web":{"image":"nginx:1"},"db":{"image":"pg:16"},"worker":{"image":"w:1"}}}`)
	rt.addC("w1", "web", "running", "healthy")
	rt.addC("d1", "db", "exited", "")
	rt.addC("o1", "old", "running", "")
	if _, err := e.svc.Expose(ctx, ExposeParams{Slug: "app", Service: "web", Port: 80, Domains: []string{"a.example.com"}}); err != nil {
		t.Fatal(err)
	}

	got, err := e.svc.Services(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	var names []string
	for _, s := range got {
		states[s.Name] = s.State
		names = append(names, s.Name)
	}
	want := map[string]string{"web": ServiceRunning, "db": ServiceStopped, "worker": ServiceMissing, "old": ServiceRunning}
	if !reflect.DeepEqual(states, want) || !reflect.DeepEqual(names, []string{"db", "old", "web", "worker"}) {
		t.Fatalf("states = %v, order %v", states, names)
	}
	web := got[2]
	if web.Image != "nginx:1" || web.Exposure == nil || web.Exposure.Port != 80 || len(web.Containers) != 1 {
		t.Fatalf("web = %+v", web)
	}
	c := web.Containers[0]
	if c.ID != "w1" || c.Health != "healthy" || c.RestartCount != 2 || c.StartedAt == nil {
		t.Fatalf("container = %+v", c)
	}
	if got[1].Image != "img-old" {
		t.Errorf("extra service image = %q", got[1].Image)
	}
	if got[3].Containers == nil {
		t.Error("containers must be an empty list, not null")
	}
	b, _ := json.Marshal(got[3])
	if !strings.Contains(string(b), `"containers":[]`) || strings.Contains(string(b), "exposure") {
		t.Errorf("json = %s", b)
	}
}

func TestServicesNoModelAndFallbackLabel(t *testing.T) {
	ctx := context.Background()
	e, rt := newSvcEnv(t)
	rt.addC("a", "web", "running", "")
	rt.addC("b", "", "running", "") // compose label only
	c := rt.list[1]
	c.Labels = map[string]string{composeServiceLabel: "cache"}
	rt.list[1] = c
	got, err := e.svc.Services(ctx, "app")
	if err != nil || len(got) != 2 || got[0].Name != "cache" || got[1].Name != "web" {
		t.Fatalf("services = %+v, %v", got, err)
	}
	if _, err := e.svc.Services(ctx, "nope"); err == nil {
		t.Fatal("unknown project should fail")
	}
}

func TestServiceActions(t *testing.T) {
	ctx := context.Background()
	e, rt := newSvcEnv(t)
	writeModel(t, e, `{"services":{"web":{"image":"nginx"},"idle":{}}}`)
	rt.addC("w1", "web", "running", "")
	rt.addC("w2", "web", "running", "")
	rt.addC("x1", "other", "running", "")

	ps, err := e.svc.StopService(ctx, "app", "web")
	if err != nil || ps.State != ServiceStopped || len(ps.Containers) != 2 {
		t.Fatalf("stop = %+v, %v", ps, err)
	}
	if !reflect.DeepEqual(rt.actions, []string{"stop:w1", "stop:w2"}) {
		t.Fatalf("actions = %v", rt.actions)
	}
	rt.actions = nil
	if ps, err = e.svc.StartService(ctx, "app", "web"); err != nil || ps.State != ServiceRunning {
		t.Fatalf("start = %+v, %v", ps, err)
	}
	if ps, err = e.svc.RestartService(ctx, "app", "web"); err != nil || ps.State != ServiceRunning {
		t.Fatalf("restart = %+v, %v", ps, err)
	}
	if !reflect.DeepEqual(rt.actions, []string{"start:w1", "start:w2", "restart:w1", "restart:w2"}) {
		t.Fatalf("actions = %v", rt.actions)
	}

	// In the model but no container: nothing to do, still found.
	rt.actions = nil
	if ps, err = e.svc.StopService(ctx, "app", "idle"); err != nil || ps.State != ServiceMissing || len(rt.actions) != 0 {
		t.Fatalf("idle = %+v, %v, %v", ps, err, rt.actions)
	}

	if _, err = e.svc.StopService(ctx, "app", "ghost"); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("ghost err = %v", err)
	}
	wantInvalidErr := func(err error) {
		t.Helper()
		var re *rpc.Error
		if !errors.As(mapError(err), &re) {
			t.Fatalf("mapError(%v) is not an rpc error", err)
		}
	}
	wantInvalidErr(ErrServiceNotFound)
	if _, err = e.svc.StopService(ctx, "app", ""); err == nil {
		t.Fatal("empty service should fail")
	}
	if _, err = e.svc.StopService(ctx, "nope", "web"); err == nil {
		t.Fatal("unknown project should fail")
	}

	// A failing container does not stop the others; the error is returned.
	rt.actions = nil
	rt.failID, rt.failErr = "w1", errors.New("boom")
	if _, err = e.svc.RestartService(ctx, "app", "web"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(rt.actions, []string{"restart:w1", "restart:w2"}) {
		t.Fatalf("actions = %v", rt.actions)
	}

	// Engine down.
	rt.err = runtime.ErrUnavailable
	_, err = e.svc.Services(ctx, "app")
	var re *rpc.Error
	if !errors.As(mapError(err), &re) || re.Code != rpc.CodeRuntimeUnavailable {
		t.Fatalf("down err = %v", err)
	}
}

func TestServiceActionSurvivesCallerCancel(t *testing.T) {
	e, rt := newSvcEnv(t)
	rt.addC("w1", "web", "running", "")
	rt.addC("w2", "web", "running", "")
	rt.addC("w3", "web", "running", "")
	ctx, cancel := context.WithCancel(context.Background())
	rt.onAct = func(string) { cancel() } // the caller disconnects after the first container
	ps, err := e.svc.RestartService(ctx, "app", "web")
	if err != nil || ps.State != ServiceRunning {
		t.Fatalf("restart = %+v, %v", ps, err)
	}
	if !reflect.DeepEqual(rt.actions, []string{"restart:w1", "restart:w2", "restart:w3"}) {
		t.Fatalf("actions = %v", rt.actions)
	}
}

func TestServiceHandlers(t *testing.T) {
	ctx := context.Background()
	e, rt := newSvcEnv(t)
	rt.addC("w1", "web", "running", "")
	h := NewHandlers(e.svc)
	marshal := func(v any) string { b, _ := json.Marshal(v); return string(b) }

	res, err := h.services(ctx, nil, json.RawMessage(`{"slug":"app"}`))
	if err != nil || !strings.HasPrefix(marshal(res), `{"services":[{"name":"web"`) {
		t.Fatalf("services = %s, %v", marshal(res), err)
	}
	res, err = h.serviceAction(e.svc.StopService)(ctx, nil, json.RawMessage(`{"slug":"app","service":"web"}`))
	if err != nil || !strings.Contains(marshal(res), `"state":"stopped"`) {
		t.Fatalf("stop = %s, %v", marshal(res), err)
	}
	var re *rpc.Error
	_, err = h.serviceAction(e.svc.StopService)(ctx, nil, json.RawMessage(`{"slug":"app","service":"nope"}`))
	if !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Fatalf("unknown service err = %v", err)
	}
	_, err = h.serviceAction(e.svc.StopService)(ctx, nil, json.RawMessage(`{"slug":"nope","service":"web"}`))
	if !errors.As(err, &re) || re.Code != rpc.CodeNotFound {
		t.Fatalf("unknown project err = %v", err)
	}
	_, err = h.services(ctx, nil, json.RawMessage(`{}`))
	if !errors.As(err, &re) || re.Code != rpc.CodeInvalidParams {
		t.Fatalf("missing slug err = %v", err)
	}
}
