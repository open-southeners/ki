package deploys

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/open-southeners/ki/internal/projects"
	"github.com/open-southeners/ki/internal/runtime"
	"github.com/open-southeners/ki/internal/store"
)

const exposedModel = `{"services":{"web":{"image":"example/web","labels":{"ki.expose.port":"8080","ki.expose.domains":"App.Example.com,www.example.com","ki.expose.path":"/x"}}}}`

func seedOther(t *testing.T, f *fixture, slug string) {
	t.Helper()
	p := &store.Project{Slug: slug, RepoURL: "https://example.com/" + slug + ".git", Branch: "main",
		AuthMethod: projects.AuthNone, WebhookID: "wh-" + slug, Status: projects.StatusReady, ComposeFile: "compose.yaml"}
	if err := f.st.CreateProject(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestDeployStoresLabelExposures(t *testing.T) {
	var mu sync.Mutex
	var finished []string
	f := newFixture(t, fixtureOpts{tweak: func(o *Options) {
		o.OnFinished = func(slug string) {
			mu.Lock()
			finished = append(finished, slug)
			mu.Unlock()
		}
	}})
	f.comp.model = exposedModel
	got := f.wait(t, f.trigger(t, "").ID)
	if got.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v (%+v)", got, got.Error)
	}
	es, err := f.st.ListExposures(context.Background(), "app")
	if err != nil || len(es) != 1 {
		t.Fatalf("exposures = %+v, %v", es, err)
	}
	e := es[0]
	if e.Service != "web" || e.Port != 8080 || e.Path != "/x" || e.Source != store.ExposureSourceLabels ||
		len(e.Domains) != 2 || e.Domains[0] != "app.example.com" {
		t.Fatalf("exposure = %+v", e)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(finished) != 1 || finished[0] != "app" {
		t.Fatalf("OnFinished = %v", finished)
	}
}

func TestLabelExposuresReplacedByNextDeploy(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.comp.model = exposedModel
	f.wait(t, f.trigger(t, "").ID)
	f.comp.model = `{"services":{"web":{"image":"example/web"}}}`
	if got := f.wait(t, f.trigger(t, "").ID); got.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v", got)
	}
	if es, _ := f.st.ListExposures(context.Background(), "app"); len(es) != 0 {
		t.Fatalf("exposures = %+v", es)
	}
}

func TestDomainConflictBeforeUp(t *testing.T) {
	var mu sync.Mutex
	var finished int
	f := newFixture(t, fixtureOpts{tweak: func(o *Options) {
		o.OnFinished = func(string) { mu.Lock(); finished++; mu.Unlock() }
	}})
	seedOther(t, f, "other")
	if _, err := f.svc.Expose(context.Background(), projects.ExposeParams{
		Slug: "other", Service: "api", Port: 80, Domains: []string{"www.example.com"}}); err != nil {
		t.Fatal(err)
	}
	f.comp.model = exposedModel
	got := f.wait(t, f.trigger(t, "").ID)
	if got.Status != StatusFailed || got.Error == nil || got.Error.Reason != ReasonDomainConflict {
		t.Fatalf("deploy = %+v (%+v)", got, got.Error)
	}
	if !strings.Contains(got.Error.Detail, "www.example.com") || !strings.Contains(got.Error.Detail, "other") {
		t.Errorf("detail = %q", got.Error.Detail)
	}
	for _, c := range f.comp.Calls() {
		if strings.HasPrefix(c, "up") || strings.HasPrefix(c, "build") {
			t.Errorf("ran %q after a domain conflict", c)
		}
	}
	if es, _ := f.st.ListExposures(context.Background(), "app"); len(es) != 0 {
		t.Fatalf("exposures = %+v", es)
	}
	mu.Lock()
	defer mu.Unlock()
	if finished != 1 {
		t.Fatalf("OnFinished fired %d times for a failed deploy", finished)
	}
}

func TestOverriddenServiceSkipsLabelConflict(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	seedOther(t, f, "other")
	ctx := context.Background()
	if _, err := f.svc.Expose(ctx, projects.ExposeParams{
		Slug: "other", Service: "api", Port: 80, Domains: []string{"www.example.com"}}); err != nil {
		t.Fatal(err)
	}
	// The project-side entry overrides the labels, so their domains are unused.
	if _, err := f.svc.Expose(ctx, projects.ExposeParams{
		Slug: "app", Service: "web", Port: 81, Domains: []string{"mine.example.com"}}); err != nil {
		t.Fatal(err)
	}
	f.comp.model = exposedModel
	if got := f.wait(t, f.trigger(t, "").ID); got.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v (%+v)", got, got.Error)
	}
	es, _ := f.st.ListExposures(ctx, "app")
	if len(es) != 1 || es[0].Source != store.ExposureSourceProject || es[0].Port != 81 {
		t.Fatalf("exposures = %+v", es)
	}
}

func TestInvalidLabelsFailCompose(t *testing.T) {
	for name, model := range map[string]string{
		"missing domains": `{"services":{"web":{"image":"x","labels":{"ki.expose.port":"80"}}}}`,
		"bad domain":      `{"services":{"web":{"image":"x","labels":{"ki.expose.port":"80","ki.expose.domains":"localhost"}}}}`,
		"bad port":        `{"services":{"web":{"image":"x","labels":{"ki.expose.port":"99999","ki.expose.domains":"a.example.com"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			f.comp.model = model
			got := f.wait(t, f.trigger(t, "").ID)
			if got.Status != StatusFailed || got.Error == nil || got.Error.Reason != ReasonComposeInvalid ||
				!strings.Contains(got.Error.Detail, "web") {
				t.Fatalf("deploy = %+v (%+v)", got, got.Error)
			}
		})
	}
}

func TestReservedLabelDomain(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.svc.SetReservedHostnames(func() []string { return []string{"www.example.com"} })
	f.comp.model = exposedModel
	got := f.wait(t, f.trigger(t, "").ID)
	if got.Status != StatusFailed || got.Error.Reason != ReasonComposeInvalid || !strings.Contains(got.Error.Detail, "reserved") {
		t.Fatalf("deploy = %+v (%+v)", got, got.Error)
	}
}

func TestConflictAfterHealthIsWarning(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	seedOther(t, f, "other")
	f.comp.model = exposedModel
	// Another project takes a domain while the deploy is between its pre-up
	// check and storing the labels (the health stage polls the runtime).
	ctx := context.Background()
	f.mgr.opts.Runtime = &hookRT{Runtime: f.mgr.opts.Runtime, onList: func() {
		_, _ = f.svc.Expose(ctx, projects.ExposeParams{
			Slug: "other", Service: "api", Port: 80, Domains: []string{"www.example.com"}})
	}}
	d := f.wait(t, f.trigger(t, "").ID)
	if d.Status != StatusSucceeded {
		t.Fatalf("deploy = %+v (%+v)", d, d.Error)
	}
	logs, _ := f.st.DeployLogs(ctx, d.ID, 0)
	var warned bool
	for _, l := range logs {
		if strings.Contains(l.Text, "warning: exposure not updated") && strings.Contains(l.Text, "www.example.com") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning in logs: %+v", logs)
	}
}

// hookRT runs onList before delegating List.
type hookRT struct {
	Runtime
	onList func()
}

func (h *hookRT) List(ctx context.Context, project string) ([]runtime.Container, error) {
	h.onList()
	return h.Runtime.List(ctx, project)
}
