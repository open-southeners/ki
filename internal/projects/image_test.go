package projects

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/store"
)

const goldenImageCompose = `services:
  app:
    image: registry.example.com/team/app:1.2.3
    restart: unless-stopped
    env_file:
      - path: .env
        required: false
    environment:
      DOMAIN: ":80"
      PRICE: "$$5 <ok>"
    expose:
      - "8080"
    volumes:
      - data:/app/data
      - logs:/var/log/app:ro
    labels:
      ki.expose.port: "8080"
      ki.expose.domains: "app.example.com,www.example.com"
      ki.expose.path: "/v1"
volumes:
  data:
  logs:
`

func TestGenerateComposeGolden(t *testing.T) {
	spec := ImageSpec{
		Image: "registry.example.com/team/app:1.2.3", Port: 8080,
		Domains: []string{"App.Example.com", "www.example.com", "app.example.com"}, Path: "/v1",
		Volumes: []string{"data:/app/data", "logs:/var/log/app:ro"},
		Env:     map[string]string{"PRICE": "$5 <ok>", "DOMAIN": ":80"},
	}
	for range 3 {
		got, err := GenerateCompose(spec)
		if err != nil || got != goldenImageCompose {
			t.Fatalf("generated:\n%s\nerr %v", got, err)
		}
	}
	minimal, err := GenerateCompose(ImageSpec{Image: "nginx:1.27", Service: "web"})
	want := "services:\n  web:\n    image: nginx:1.27\n    restart: unless-stopped\n    env_file:\n      - path: .env\n        required: false\n"
	if err != nil || minimal != want {
		t.Errorf("minimal:\n%s\nerr %v", minimal, err)
	}
}

func TestGenerateComposeInvalid(t *testing.T) {
	ok := "nginx:1.27"
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name string
		spec ImageSpec
		want string
	}{
		{"no image", ImageSpec{}, "image is required"},
		{"untagged", ImageSpec{Image: "nginx"}, "pin a tag or digest"},
		{"latest", ImageSpec{Image: "ghcr.io/acme/web:latest"}, "pin a tag or digest"},
		{"upper repo", ImageSpec{Image: "ghcr.io/Acme/web:1"}, "invalid image name"},
		{"bad tag", ImageSpec{Image: "nginx:-1"}, "invalid image tag"},
		{"bad digest", ImageSpec{Image: "nginx@sha256:abc"}, "invalid image digest"},
		{"bad service", ImageSpec{Image: ok, Service: "Web"}, "service must match"},
		{"port range", ImageSpec{Image: ok, Port: 70000}, "port must be between"},
		{"neg port", ImageSpec{Image: ok, Port: -1}, "port must be between"},
		{"domains no port", ImageSpec{Image: ok, Domains: []string{"a.example.com"}}, "domains require port"},
		{"path no domains", ImageSpec{Image: ok, Port: 80, Path: "/x"}, "path requires domains"},
		{"bad domain", ImageSpec{Image: ok, Port: 80, Domains: []string{"*.example.com"}}, "invalid domain"},
		{"bad path", ImageSpec{Image: ok, Port: 80, Domains: []string{"a.example.com"}, Path: "x"}, "path must start with /"},
		{"bind mount", ImageSpec{Image: ok, Volumes: []string{"/srv/data:/data"}}, "named volume"},
		{"relative bind", ImageSpec{Image: ok, Volumes: []string{"./data:/data"}}, "named volume"},
		{"no path", ImageSpec{Image: ok, Volumes: []string{"data"}}, "invalid volume"},
		{"rel target", ImageSpec{Image: ok, Volumes: []string{"data:app"}}, "absolute"},
		{"root target", ImageSpec{Image: ok, Volumes: []string{"data:/"}}, "absolute"},
		{"dotdot target", ImageSpec{Image: ok, Volumes: []string{"data:/a/../b"}}, "absolute"},
		{"bad mode", ImageSpec{Image: ok, Volumes: []string{"data:/d:rw"}}, "only option is ro"},
		{"dup target", ImageSpec{Image: ok, Volumes: []string{"a:/d", "b:/d"}}, "mounted twice"},
		{"env key", ImageSpec{Image: ok, Env: map[string]string{"1BAD": "x"}}, "invalid env name"},
		{"env key dash", ImageSpec{Image: ok, Env: map[string]string{"A-B": "x"}}, "invalid env name"},
		{"env multiline", ImageSpec{Image: ok, Env: map[string]string{"A": "x\ny"}}, "single line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := GenerateCompose(tt.spec)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(inv.Msg, tt.want) {
				t.Fatalf("err = %v, want invalid containing %q", err, tt.want)
			}
		})
	}
	if _, err := GenerateCompose(ImageSpec{Image: "localhost:5000/app@" + digest}); err != nil {
		t.Errorf("digest ref: %v", err)
	}
}

func TestParseImageRef(t *testing.T) {
	for _, ref := range []string{"nginx:1.27", "acme/web:2", "ghcr.io/acme/web@sha256:" + strings.Repeat("0", 64),
		"localhost:5000/a/b_c.d--e:v1_2", "registry.example.com:5443/team/app:1.2.3"} {
		r, err := compose.ParsePinnedImageRef(ref)
		if err != nil || r.String() != ref {
			t.Errorf("%s: %+v, %v", ref, r, err)
		}
	}
	r, _ := compose.ParseImageRef("registry.example.com:5443/team/app:1")
	if r.Registry != "registry.example.com:5443" || r.Path != "team/app" || r.Tag != "1" {
		t.Errorf("parsed = %+v", r)
	}
	for _, ref := range []string{"", "nginx", "nginx:latest", "a//b:1", "Nginx:1", "nginx:", "x:1@sha256:zz", "-a/b:1", "a b:1"} {
		if _, err := compose.ParsePinnedImageRef(ref); err == nil {
			t.Errorf("%q accepted", ref)
		}
	}
}

func TestCreateImage(t *testing.T) {
	ctx := context.Background()
	e, fv := newComposeEnv(t)
	res, err := e.svc.Create(ctx, CreateParams{Slug: "web", CreatedBy: "alice", Image: &ImageSpec{
		Image: "registry.example.com/team/app:1.2.3", Port: 8080, Domains: []string{"app.example.com"},
		Volumes: []string{"data:/app/data"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Project.Source != SourceCompose || res.Project.ComposeVersion != 1 || res.WebhookSecret != "" {
		t.Errorf("result = %+v", res)
	}
	if len(fv.calls) != 1 || fv.calls[0] != "web" {
		t.Errorf("validator calls = %v", fv.calls)
	}
	cv, err := e.svc.ComposeGet(ctx, "web", 1)
	if err != nil || !strings.Contains(cv.Content, "image: registry.example.com/team/app:1.2.3\n") ||
		!strings.Contains(cv.Content, `ki.expose.domains: "app.example.com"`) || cv.CreatedBy != "alice" {
		t.Errorf("v1 = %+v, %v", cv, err)
	}
	if cv.Note != imageNote {
		t.Errorf("note = %q, want %q", cv.Note, imageNote)
	}

	tests := []struct {
		name string
		in   CreateParams
		want string
	}{
		{"with compose", CreateParams{Slug: "a", Compose: goodCompose, Image: &ImageSpec{Image: "nginx:1"}}, "mutually exclusive"},
		{"with repo", CreateParams{Slug: "a", RepoURL: "https://example.com/a.git", Image: &ImageSpec{Image: "nginx:1"}}, "mutually exclusive"},
		{"no slug", CreateParams{Image: &ImageSpec{Image: "nginx:1"}}, "slug is required"},
		{"latest", CreateParams{Slug: "a", Image: &ImageSpec{Image: "nginx:latest"}}, "pin a tag or digest"},
		{"git field", CreateParams{Slug: "a", Branch: "main", Image: &ImageSpec{Image: "nginx:1"}}, "only to git"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.Create(ctx, tt.in)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(inv.Msg, tt.want) {
				t.Fatalf("err = %v, want invalid containing %q", err, tt.want)
			}
		})
	}
	// A reserved hostname is refused.
	e.svc.SetReservedHostnames(func() []string { return []string{"panel.example.com"} })
	_, err = e.svc.Create(ctx, CreateParams{Slug: "b", Image: &ImageSpec{Image: "nginx:1", Port: 80, Domains: []string{"panel.example.com"}}})
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(inv.Msg, "reserved") {
		t.Errorf("reserved err = %v", err)
	}
}

const commentedCompose = `# my stack
services:
  web:
    image: registry.example.com/team/app:1.0.0 # the app
    ports: [] # none
  db:
    image: postgres:16
`

func TestImageSet(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	if _, err := e.svc.Create(ctx, CreateParams{Slug: "api", Compose: goodCompose, CreatedBy: "alice"}); err != nil {
		t.Fatal(err)
	}

	// By tag: keeps the repository, the only service with an image is the default.
	res, err := e.svc.ImageSet(ctx, ImageSetParams{Slug: "api", Tag: "2", CreatedBy: "bob"})
	if err != nil || res.Version != 2 || res.Image != "registry.example/app:2" || res.Deploy != nil {
		t.Fatalf("by tag = %+v, %v", res, err)
	}
	cv, _ := e.svc.ComposeGet(ctx, "api", 0)
	if cv.Content != "services:\n  web:\n    image: registry.example/app:2\n" || cv.Note != "image registry.example/app:2" || cv.CreatedBy != "bob" {
		t.Errorf("v2 = %+v", cv)
	}

	// By digest as the tag, then by full ref with a note.
	d := "sha256:" + strings.Repeat("b", 64)
	if res, err = e.svc.ImageSet(ctx, ImageSetParams{Slug: "api", Tag: d}); err != nil || res.Image != "registry.example/app@"+d {
		t.Fatalf("by digest = %+v, %v", res, err)
	}
	res, err = e.svc.ImageSet(ctx, ImageSetParams{Slug: "api", Image: "ghcr.io/acme/web:3", Note: "switch registry"})
	if err != nil || res.Version != 4 || res.Image != "ghcr.io/acme/web:3" {
		t.Fatalf("by ref = %+v, %v", res, err)
	}
	if cv, _ = e.svc.ComposeGet(ctx, "api", 4); cv.Note != "switch registry" {
		t.Errorf("note = %q", cv.Note)
	}

	// Refusals store nothing.
	tests := []struct {
		name string
		in   ImageSetParams
		want string
	}{
		{"both", ImageSetParams{Slug: "api", Image: "nginx:1", Tag: "2"}, "mutually exclusive"},
		{"neither", ImageSetParams{Slug: "api"}, "image or tag is required"},
		{"latest tag", ImageSetParams{Slug: "api", Tag: "latest"}, "pin a tag or digest"},
		{"latest ref", ImageSetParams{Slug: "api", Image: "nginx:latest"}, "pin a tag or digest"},
		{"untagged ref", ImageSetParams{Slug: "api", Image: "nginx"}, "pin a tag or digest"},
		{"bad tag", ImageSetParams{Slug: "api", Tag: "a b"}, "invalid image tag"},
		{"unknown service", ImageSetParams{Slug: "api", Tag: "2", Service: "nope"}, "services with an image: web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.ImageSet(ctx, tt.in)
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(inv.Msg, tt.want) {
				t.Fatalf("err = %v, want invalid containing %q", err, tt.want)
			}
		})
	}
	if n, _ := e.st.LatestComposeVersion(ctx, "api"); n != 4 {
		t.Errorf("latest = %d, want 4", n)
	}
	if _, err := e.svc.ImageSet(ctx, ImageSetParams{Slug: "nope", Tag: "2"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown project err = %v", err)
	}
}

func TestImageSetMultiServiceAndComments(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	if _, err := e.svc.Create(ctx, CreateParams{Slug: "stack", Compose: commentedCompose}); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.ImageSet(ctx, ImageSetParams{Slug: "stack", Tag: "2"})
	var inv *InvalidError
	if !errors.As(err, &inv) || !strings.Contains(inv.Msg, "web, db") {
		t.Fatalf("ambiguous err = %v", err)
	}
	res, err := e.svc.ImageSet(ctx, ImageSetParams{Slug: "stack", Tag: "1.1.0", Service: "web"})
	if err != nil || res.Image != "registry.example.com/team/app:1.1.0" {
		t.Fatalf("set = %+v, %v", res, err)
	}
	cv, _ := e.svc.ComposeGet(ctx, "stack", 0)
	want := strings.Replace(commentedCompose, "app:1.0.0", "app:1.1.0", 1)
	if cv.Content != want {
		t.Errorf("content:\n%s\nwant:\n%s", cv.Content, want)
	}
	if res, err = e.svc.ImageSet(ctx, ImageSetParams{Slug: "stack", Image: "postgres:17", Service: "db"}); err != nil || res.Version != 3 {
		t.Errorf("db = %+v, %v", res, err)
	}
}

func TestImageSetGitProjectAndValidation(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	e.seedProject(t, "app")
	var inv *InvalidError
	if _, err := e.svc.ImageSet(ctx, ImageSetParams{Slug: "app", Tag: "2"}); !errors.As(err, &inv) || inv.Msg != "not a compose project" {
		t.Errorf("git err = %v", err)
	}
	// The generated file goes through the validator: a rejection stores nothing.
	createCompose(t, e, "api")
	fv := &fakeValidator{err: errors.New("down")}
	e.svc.validateCompose = fv.validate
	if _, err := e.svc.ImageSet(ctx, ImageSetParams{Slug: "api", Tag: "2"}); err == nil || err.Error() != "down" {
		t.Errorf("validator err = %v", err)
	}
	if n, _ := e.st.LatestComposeVersion(ctx, "api"); n != 1 {
		t.Errorf("latest = %d", n)
	}
}

func TestImageSetHandlerDeploy(t *testing.T) {
	ctx := context.Background()
	e, _ := newComposeEnv(t)
	createCompose(t, e, "api")
	h := NewHandlers(e.svc)
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

	if _, err := h.imageSet(ctx, nil, raw(map[string]any{"tag": "2"})); err == nil {
		t.Error("missing slug accepted")
	}
	if _, err := h.imageSet(ctx, nil, raw(map[string]any{"slug": "api", "tag": "2", "deploy": true})); err == nil {
		t.Fatal("deploy without hook succeeded")
	}
	var calls []int
	e.svc.SetOnComposeSet(func(_ context.Context, slug string, version int) (any, error) {
		calls = append(calls, version)
		return map[string]any{"id": 9}, nil
	})
	got, err := h.imageSet(ctx, nil, raw(map[string]any{"slug": "api", "tag": "3", "deploy": true}))
	b, _ := json.Marshal(got)
	if err != nil || string(b) != `{"version":2,"image":"registry.example/app:3","deploy":{"id":9}}` {
		t.Fatalf("set = %s, %v", b, err)
	}
	if len(calls) != 1 || calls[0] != 2 {
		t.Errorf("hook calls = %v", calls)
	}
	if _, err := h.imageSet(ctx, nil, raw(map[string]any{"slug": "api", "tag": "4"})); err != nil || len(calls) != 1 {
		t.Errorf("hook called without deploy: %v %v", calls, err)
	}
	_, err = h.imageSet(ctx, nil, raw(map[string]any{"slug": "api", "tag": "latest"}))
	if err == nil || !strings.Contains(err.Error(), "invalid params") {
		t.Errorf("latest err = %v", err)
	}
	// create over the handler with image.
	if _, err := h.create(ctx, nil, raw(map[string]any{"slug": "img", "image": map[string]any{"image": "nginx:1.27", "port": 80}})); err != nil {
		t.Errorf("create image: %v", err)
	}
}
