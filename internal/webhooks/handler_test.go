package webhooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/store"
)

var secret = []byte("s3cret-value")

type fakeProjects struct {
	hooks map[string]Hook
	err   error
}

func (f *fakeProjects) ByWebhookID(_ context.Context, id string) (Hook, error) {
	if f.err != nil {
		return Hook{}, f.err
	}
	h, ok := f.hooks[id]
	if !ok {
		return Hook{}, store.ErrNotFound
	}
	return h, nil
}

type fakeDeployer struct {
	mu   sync.Mutex
	reqs []TriggerRequest
	err  error
}

func (f *fakeDeployer) Trigger(_ context.Context, req TriggerRequest) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	f.reqs = append(f.reqs, req)
	return int64(len(f.reqs)), nil
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type env struct {
	h *Handler
	d *fakeDeployer
	p *fakeProjects
}

func newEnv(max int64) *env {
	p := &fakeProjects{hooks: map[string]Hook{
		"br":  {Slug: "app", Branch: "main", Status: "ready", Secret: secret},
		"tag": {Slug: "app", TagPattern: "v*", Status: "ready", Secret: secret},
		"pa":  {Slug: "app", Branch: "main", Status: "pending_access", Secret: secret},
	}}
	d := &fakeDeployer{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &env{h: NewHandler(Options{Projects: p, Deployer: d, MaxBodyBytes: max, Logger: logger}), d: d, p: p}
}

func (e *env) do(method, target string, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func gh(event, delivery, body string) map[string]string {
	h := map[string]string{"X-GitHub-Event": event, "X-Hub-Signature-256": sign([]byte(body))}
	if delivery != "" {
		h["X-GitHub-Delivery"] = delivery
	}
	return h
}

func TestGitHub(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	push := `{"ref":"refs/heads/main","after":"` + sha + `","deleted":false}`
	other := `{"ref":"refs/heads/dev","after":"` + sha + `"}`
	deleted := `{"ref":"refs/heads/main","after":"` + sha + `","deleted":true}`
	zero := `{"ref":"refs/heads/main","after":"` + zeroSHA + `"}`
	tagOK := `{"ref":"refs/tags/v1.2.0","after":"` + sha + `"}`
	const tagObj = "4444444444444444444444444444444444444444"
	tagHead := `{"ref":"refs/tags/v1.3.0","after":"` + tagObj + `","head_commit":{"id":"` + sha + `"}}`
	tagBare := `{"ref":"refs/tags/v1.4.0","after":"` + tagObj + `"}`
	tagNo := `{"ref":"refs/tags/rc1","after":"` + sha + `"}`
	tests := []struct {
		name   string
		target string
		body   string
		hdr    map[string]string
		code   int
		want   string // substring of response body
		deploy *TriggerRequest
	}{
		{"valid push", "/hooks/br", push, gh("push", "d1", push), 202, `"deploy":1`,
			&TriggerRequest{Project: "app", Ref: "refs/heads/main", Commit: sha, Source: "webhook"}},
		{"ping", "/hooks/br", `{}`, gh("ping", "", `{}`), 200, `"ok":true`, nil},
		{"ping bad sig", "/hooks/br", `{}`, map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": "sha256=00"}, 401, "", nil},
		{"missing signature", "/hooks/br", push, map[string]string{"X-GitHub-Event": "push"}, 401, "", nil},
		{"wrong signature", "/hooks/br", push, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": sign([]byte("other"))}, 401, "", nil},
		{"non-hex signature", "/hooks/br", push, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=zz"}, 401, "", nil},
		{"no prefix signature", "/hooks/br", push, map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": strings.TrimPrefix(sign([]byte(push)), "sha256=")}, 401, "", nil},
		{"other event", "/hooks/br", `{}`, gh("issues", "", `{}`), 202, `"ignored":true`, nil},
		{"other branch", "/hooks/br", other, gh("push", "", other), 202, `"ignored":true`, nil},
		{"deleted flag", "/hooks/br", deleted, gh("push", "", deleted), 202, `"ignored":true`, nil},
		{"zero after", "/hooks/br", zero, gh("push", "", zero), 202, `"ignored":true`, nil},
		{"tag match", "/hooks/tag", tagOK, gh("push", "", tagOK), 202, `"deploy":1`,
			&TriggerRequest{Project: "app", Ref: "refs/tags/v1.2.0", Commit: "", Source: "webhook"}},
		{"annotated tag uses head_commit", "/hooks/tag", tagHead, gh("push", "", tagHead), 202, `"deploy":1`,
			&TriggerRequest{Project: "app", Ref: "refs/tags/v1.3.0", Commit: sha, Source: "webhook"}},
		{"tag without head_commit leaves commit empty", "/hooks/tag", tagBare, gh("push", "", tagBare), 202, `"deploy":1`,
			&TriggerRequest{Project: "app", Ref: "refs/tags/v1.4.0", Commit: "", Source: "webhook"}},
		{"tag no match", "/hooks/tag", tagNo, gh("push", "", tagNo), 202, `"ignored":true`, nil},
		{"branch push on tag trigger", "/hooks/tag", push, gh("push", "", push), 202, `"ignored":true`, nil},
		{"pending project", "/hooks/pa", push, gh("push", "", push), 409, "", nil},
		{"unknown id", "/hooks/nope", push, gh("push", "", push), 404, "", nil},
		{"no source headers", "/hooks/br", push, map[string]string{}, 400, "", nil},
		{"invalid json", "/hooks/br", `{`, gh("push", "", `{`), 400, "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(0)
			w := e.do("POST", tc.target, tc.body, tc.hdr)
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.code, w.Body)
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Errorf("body %q lacks %q", w.Body, tc.want)
			}
			if strings.Contains(w.Body.String(), string(secret)) {
				t.Error("response leaks secret")
			}
			switch {
			case tc.deploy == nil && len(e.d.reqs) != 0:
				t.Errorf("unexpected trigger: %+v", e.d.reqs)
			case tc.deploy != nil && (len(e.d.reqs) != 1 || e.d.reqs[0] != *tc.deploy):
				t.Errorf("triggers = %+v, want %+v", e.d.reqs, *tc.deploy)
			}
		})
	}
}

func TestGitLab(t *testing.T) {
	const sha = "2222222222222222222222222222222222222222"
	const tagObj = "3333333333333333333333333333333333333333"
	branch := `{"ref":"refs/heads/main","after":"` + sha + `","checkout_sha":"` + sha + `"}`
	tagPush := `{"ref":"refs/tags/v2.0.0","after":"` + tagObj + `","checkout_sha":"` + sha + `"}`
	del := `{"ref":"refs/heads/main","after":"` + zeroSHA + `","checkout_sha":null}`
	ok := map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": string(secret)}
	tests := []struct {
		name   string
		target string
		body   string
		hdr    map[string]string
		code   int
		commit string
	}{
		{"push", "/hooks/br", branch, ok, 202, sha},
		{"tag push uses checkout_sha", "/hooks/tag", tagPush, map[string]string{"X-Gitlab-Event": "Tag Push Hook", "X-Gitlab-Token": string(secret)}, 202, sha},
		{"bad token", "/hooks/br", branch, map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "nope"}, 401, ""},
		{"missing token", "/hooks/br", branch, map[string]string{"X-Gitlab-Event": "Push Hook"}, 401, ""},
		{"other event", "/hooks/br", branch, map[string]string{"X-Gitlab-Event": "Issue Hook", "X-Gitlab-Token": string(secret)}, 202, ""},
		{"deletion", "/hooks/br", del, ok, 202, ""},
		{"pending", "/hooks/pa", branch, ok, 409, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(0)
			w := e.do("POST", tc.target, tc.body, tc.hdr)
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.code, w.Body)
			}
			if tc.commit == "" {
				if len(e.d.reqs) != 0 {
					t.Fatalf("unexpected trigger %+v", e.d.reqs)
				}
				return
			}
			if len(e.d.reqs) != 1 || e.d.reqs[0].Commit != tc.commit {
				t.Fatalf("triggers = %+v, want commit %s", e.d.reqs, tc.commit)
			}
		})
	}
}

func TestDedupe(t *testing.T) {
	body := `{"ref":"refs/heads/main","after":"1111111111111111111111111111111111111111"}`
	e := newEnv(0)
	if w := e.do("POST", "/hooks/br", body, gh("push", "dup", body)); w.Code != 202 {
		t.Fatalf("first = %d", w.Code)
	}
	w := e.do("POST", "/hooks/br", body, gh("push", "dup", body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "duplicate") {
		t.Fatalf("second = %d %s", w.Code, w.Body)
	}
	if len(e.d.reqs) != 1 {
		t.Fatalf("triggers = %d, want 1", len(e.d.reqs))
	}

	gl := map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": string(secret), "X-Gitlab-Event-UUID": "u1"}
	e.do("POST", "/hooks/br", body, gl)
	if w := e.do("POST", "/hooks/br", body, gl); w.Code != 200 {
		t.Fatalf("gitlab duplicate = %d", w.Code)
	}

	// A failed trigger is not remembered, so the redelivery is retried.
	e = newEnv(0)
	e.d.err = errors.New("boom")
	if w := e.do("POST", "/hooks/br", body, gh("push", "r", body)); w.Code != 500 {
		t.Fatalf("failed = %d", w.Code)
	}
	e.d.err = nil
	if w := e.do("POST", "/hooks/br", body, gh("push", "r", body)); w.Code != 202 {
		t.Fatalf("retry = %d", w.Code)
	}
}

func TestDedupeLRUAndTTL(t *testing.T) {
	d := newDedupe(2, time.Hour)
	now := time.Unix(0, 0)
	d.now = func() time.Time { return now }
	d.add("a")
	d.add("b")
	d.has("a") // a is now most recent
	d.add("c") // evicts b
	if !d.has("a") || d.has("b") || !d.has("c") {
		t.Fatal("LRU eviction wrong")
	}
	now = now.Add(2 * time.Hour)
	if d.has("a") {
		t.Fatal("entry should have expired")
	}
}

func TestBodyCap(t *testing.T) {
	e := newEnv(64)
	body := `{"ref":"refs/heads/main","pad":"` + strings.Repeat("x", 200) + `"}`
	if w := e.do("POST", "/hooks/br", body, gh("push", "", body)); w.Code != 413 {
		t.Fatalf("code = %d, want 413", w.Code)
	}
}

func TestDeployerErrorHidesDetail(t *testing.T) {
	e := newEnv(0)
	e.d.err = errors.New("internal path /var/lib/ki secret=hunter2")
	body := `{"ref":"refs/heads/main","after":"1111111111111111111111111111111111111111"}`
	w := e.do("POST", "/hooks/br", body, gh("push", "", body))
	if w.Code != 500 {
		t.Fatalf("code = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "/var/lib") {
		t.Errorf("leaked detail: %s", w.Body)
	}
}

func TestLookupErrorIs500(t *testing.T) {
	e := newEnv(0)
	e.p.err = errors.New("db down")
	if w := e.do("POST", "/hooks/br", "{}", gh("push", "", "{}")); w.Code != 500 || strings.Contains(w.Body.String(), "db") {
		t.Fatalf("code = %d body %s", w.Code, w.Body)
	}
}

func TestRouting(t *testing.T) {
	e := newEnv(0)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/hooks/br"}, {"POST", "/"}, {"POST", "/hooks/"}, {"POST", "/hooks/br/x"}, {"POST", "/other"},
	} {
		if w := e.do(tc.method, tc.target, "{}", gh("push", "", "{}")); w.Code != 404 {
			t.Errorf("%s %s = %d, want 404", tc.method, tc.target, w.Code)
		}
	}
}

func TestServerShutdown(t *testing.T) {
	e := newEnv(0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{Handler: e.h}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	body := `{"ref":"refs/heads/main","after":"1111111111111111111111111111111111111111"}`
	req, _ := http.NewRequest("POST", "http://"+ln.Addr().String()+"/hooks/br", bytes.NewReader([]byte(body)))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-Hub-Signature-256", sign([]byte(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 202 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown timed out")
	}
}
