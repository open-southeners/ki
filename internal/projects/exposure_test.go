package projects

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/store"
)

func TestValidateExposure(t *testing.T) {
	long := strings.Repeat("a", 64)
	many := make([]string, 21)
	for i := range many {
		many[i] = "d" + strings.Repeat("x", i) + ".example.com"
	}
	tests := []struct {
		name    string
		service string
		port    int
		domains []string
		path    string
		ok      bool
	}{
		{"ok", "web", 8080, []string{"app.example.com"}, "/api", true},
		{"no path", "web", 80, []string{"app.example.com"}, "", true},
		{"punycode", "web", 80, []string{"xn--bcher-kva.example"}, "", true},
		{"port zero", "web", 0, []string{"app.example.com"}, "", false},
		{"port high", "web", 65536, []string{"app.example.com"}, "", false},
		{"no domains", "web", 80, nil, "", false},
		{"too many", "web", 80, many, "", false},
		{"bad service", "we b", 80, []string{"app.example.com"}, "", false},
		{"empty service", "", 80, []string{"app.example.com"}, "", false},
		{"no dot", "web", 80, []string{"localhost"}, "", false},
		{"wildcard", "web", 80, []string{"*.example.com"}, "", false},
		{"ipv4", "web", 80, []string{"10.0.0.1"}, "", false},
		{"ipv6", "web", 80, []string{"::1"}, "", false},
		{"unicode", "web", 80, []string{"bücher.example"}, "", false},
		{"long label", "web", 80, []string{long + ".example.com"}, "", false},
		{"long name", "web", 80, []string{strings.Repeat("a.", 130) + "com"}, "", false},
		{"leading hyphen", "web", 80, []string{"-a.example.com"}, "", false},
		{"empty label", "web", 80, []string{"a..example.com"}, "", false},
		{"trailing dot", "web", 80, []string{"a.example.com."}, "", false},
		{"numeric tld", "web", 80, []string{"a.123"}, "", false},
		{"path no slash", "web", 80, []string{"a.example.com"}, "api", false},
		{"path dotdot", "web", 80, []string{"a.example.com"}, "/a/../b", false},
		{"path space", "web", 80, []string{"a.example.com"}, "/a b", false},
		{"path long", "web", 80, []string{"a.example.com"}, "/" + strings.Repeat("a", 256), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateExposure(tt.service, tt.port, tt.domains, tt.path)
			if tt.ok != (err == nil) {
				t.Fatalf("err = %v, ok = %v", err, tt.ok)
			}
			if err != nil {
				wantInvalid(t, err)
			}
		})
	}
	got, err := ValidateExposure("web", 80, []string{" App.Example.com ", "app.example.com", "b.example.com"}, "")
	if err != nil || len(got) != 2 || got[0] != "app.example.com" || got[1] != "b.example.com" {
		t.Fatalf("normalize = %v, %v", got, err)
	}
}

func TestExposeReservedHostnames(t *testing.T) {
	e := newEnv(t)
	e.seedProject(t, "app")
	e.svc.SetReservedHostnames(func() []string { return []string{"Edge.Example.com"} })
	_, err := e.svc.Expose(context.Background(), ExposeParams{Slug: "app", Service: "web", Port: 80, Domains: []string{"edge.example.com"}})
	wantInvalid(t, err)
	if _, err := e.svc.Expose(context.Background(), ExposeParams{Slug: "app", Service: "web", Port: 80, Domains: []string{"ok.example.com"}}); err != nil {
		t.Fatal(err)
	}
}

func TestExposeUnexposeAndHook(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "app")
	var fired atomic.Int32
	e.svc.OnExposureChange(func() { fired.Add(1) })

	p, err := e.svc.Expose(ctx, ExposeParams{Slug: "app", Service: "web", Port: 8080, Domains: []string{"A.example.com"}, Path: "/x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Exposure) != 1 || p.Exposure[0].Domains[0] != "a.example.com" || p.Exposure[0].Source != "project" || p.Exposure[0].Path != "/x" {
		t.Fatalf("exposure = %+v", p.Exposure)
	}
	// Never deployed: the service cannot be verified.
	if len(p.ExposureWarnings) != 1 || !strings.Contains(p.ExposureWarnings[0], "web") {
		t.Fatalf("warnings = %v", p.ExposureWarnings)
	}
	if fired.Load() != 1 {
		t.Fatalf("hook fired %d", fired.Load())
	}
	// Re-expose replaces.
	if p, err = e.svc.Expose(ctx, ExposeParams{Slug: "app", Service: "web", Port: 9000, Domains: []string{"b.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if len(p.Exposure) != 1 || p.Exposure[0].Port != 9000 {
		t.Fatalf("exposure = %+v", p.Exposure)
	}
	got, err := e.svc.Get(ctx, "app")
	if err != nil || len(got.Exposure) != 1 {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if p, err = e.svc.Unexpose(ctx, "app", "web"); err != nil {
		t.Fatal(err)
	}
	if len(p.Exposure) != 0 || len(p.ExposureWarnings) != 0 {
		t.Fatalf("after unexpose: %+v", p)
	}
	if fired.Load() != 3 {
		t.Fatalf("hook fired %d", fired.Load())
	}
	// Failed calls do not fire.
	if _, err := e.svc.Expose(ctx, ExposeParams{Slug: "app", Service: "web", Port: 0, Domains: []string{"a.example.com"}}); err == nil {
		t.Fatal("want error")
	}
	if _, err := e.svc.Expose(ctx, ExposeParams{Slug: "nope", Service: "web", Port: 80, Domains: []string{"a.example.com"}}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if fired.Load() != 3 {
		t.Fatalf("hook fired %d", fired.Load())
	}
	b, _ := json.Marshal(got)
	if !strings.Contains(string(b), `"exposure"`) {
		t.Fatalf("json = %s", b)
	}
	b, _ = json.Marshal(p)
	if strings.Contains(string(b), "exposure") {
		t.Fatalf("empty exposure must be omitted: %s", b)
	}
}

func TestExposeConflict(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "one")
	e.seedProject(t, "two")
	if _, err := e.svc.Expose(ctx, ExposeParams{Slug: "one", Service: "web", Port: 80, Domains: []string{"a.example.com"}}); err != nil {
		t.Fatal(err)
	}
	var fired atomic.Int32
	e.svc.OnExposureChange(func() { fired.Add(1) })
	_, err := e.svc.Expose(ctx, ExposeParams{Slug: "two", Service: "web", Port: 80, Domains: []string{"x.example.com", "a.example.com"}})
	var dc *store.DomainConflictError
	if !errors.As(err, &dc) || dc.Domain != "a.example.com" || dc.Project != "one" {
		t.Fatalf("err = %v", err)
	}
	var re *rpc.Error
	if !errors.As(mapError(err), &re) || re.Code != rpc.CodeAlreadyExists {
		t.Fatalf("mapError = %v", mapError(err))
	}
	if fired.Load() != 0 {
		t.Fatal("hook fired on conflict")
	}
	if p, _ := e.svc.Get(ctx, "two"); len(p.Exposure) != 0 {
		t.Fatalf("conflict changed state: %+v", p.Exposure)
	}
}

func TestExposeOverridesLabelsAndWarnings(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seedProject(t, "one")
	e.seedProject(t, "two")
	// A deployed model with the services web and api.
	dir := filepath.Join(e.data, "deploys", "one")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.json"), []byte(`{"services":{"web":{},"api":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.st.ReplaceLabelExposures(ctx, "one", []store.Exposure{
		{Service: "web", Port: 80, Domains: []string{"l.example.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	p, _ := e.svc.Get(ctx, "one")
	if len(p.Exposure) != 1 || p.Exposure[0].Source != "labels" || len(p.ExposureWarnings) != 0 {
		t.Fatalf("labels view: %+v", p)
	}
	// Unknown service in a known model is rejected.
	_, err := e.svc.Expose(ctx, ExposeParams{Slug: "one", Service: "ghost", Port: 80, Domains: []string{"g.example.com"}})
	wantInvalid(t, err)
	// Override.
	p, err = e.svc.Expose(ctx, ExposeParams{Slug: "one", Service: "web", Port: 81, Domains: []string{"o.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Exposure) != 1 || p.Exposure[0].Source != "project" || p.Exposure[0].Port != 81 || len(p.ExposureWarnings) != 0 {
		t.Fatalf("override view: %+v", p)
	}
	// Another project takes the label domain while it is overridden, then the
	// override is removed: the label domain stays unrouted and is reported.
	if _, err := e.svc.Expose(ctx, ExposeParams{Slug: "two", Service: "web", Port: 80, Domains: []string{"l.example.com"}}); err != nil {
		t.Fatal(err)
	}
	p, err = e.svc.Unexpose(ctx, "one", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Exposure) != 1 || p.Exposure[0].Source != "labels" {
		t.Fatalf("after unexpose: %+v", p.Exposure)
	}
	if len(p.ExposureWarnings) != 1 || !strings.Contains(p.ExposureWarnings[0], "l.example.com") ||
		!strings.Contains(p.ExposureWarnings[0], "two/web") {
		t.Fatalf("warnings = %v", p.ExposureWarnings)
	}
	// Unexposing a service without a project entry is a no-op.
	if _, err := e.svc.Unexpose(ctx, "one", "api"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Unexpose(ctx, "one", ""); err == nil {
		t.Fatal("empty service accepted")
	}
}
