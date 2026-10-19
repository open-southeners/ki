package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type flow struct {
	t   *testing.T
	srv *httptest.Server
	en  *enroller
	clk *fakeClock
}

func newFlow(t *testing.T) *flow {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	en := newEnroller("ws://127.0.0.1:8080/agent", clk.now)
	mux := http.NewServeMux()
	en.register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &flow{t: t, srv: srv, en: en, clk: clk}
}

func (f *flow) post(path string, form url.Values) (int, map[string]any) {
	f.t.Helper()
	resp, err := http.PostForm(f.srv.URL+path, form)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func (f *flow) start() (device, user string) {
	f.t.Helper()
	code, m := f.post("/enroll/device", url.Values{
		"client_id": {"ki-agent"}, "hostname": {"box"}, "os": {"linux"}, "arch": {"amd64"},
	})
	if code != 200 {
		f.t.Fatalf("device authorization: %d %v", code, m)
	}
	return m["device_code"].(string), m["user_code"].(string)
}

func (f *flow) poll(device string) (int, map[string]any) {
	return f.post("/enroll/token", url.Values{
		"grant_type": {grantDeviceCode}, "device_code": {device}, "client_id": {"ki-agent"},
	})
}

func (f *flow) activate(user, action string) int {
	f.t.Helper()
	resp, err := http.PostForm(f.srv.URL+"/activate", url.Values{"user_code": {user}, "action": {action}})
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestDiscovery(t *testing.T) {
	f := newFlow(t)
	resp, err := http.Get(f.srv.URL + "/.well-known/ki-agent-enrollment")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if m["device_authorization_endpoint"] != f.srv.URL+"/enroll/device" || m["token_endpoint"] != f.srv.URL+"/enroll/token" {
		t.Fatalf("discovery = %v", m)
	}
}

func TestDeviceFlowHappyPath(t *testing.T) {
	f := newFlow(t)
	device, user := f.start()
	if !regexp.MustCompile(`^[A-Z0-9]{4}-[A-Z0-9]{4}$`).MatchString(user) {
		t.Fatalf("user code %q", user)
	}
	if len(device) < 43 { // 32 bytes, base64url
		t.Fatalf("device code too short: %q", device)
	}

	if code, m := f.poll(device); code != 400 || m["error"] != "authorization_pending" {
		t.Fatalf("pending: %d %v", code, m)
	}
	// Lower case and no dash are fine when typing the code.
	if code := f.activate(strings.ToLower(strings.ReplaceAll(user, "-", "")), "approve"); code != 200 {
		t.Fatalf("activate: %d", code)
	}
	f.clk.advance(pollInterval)
	code, m := f.poll(device)
	if code != 200 || m["token_type"] != "Bearer" || m["control_plane_url"] != "ws://127.0.0.1:8080/agent" {
		t.Fatalf("token: %d %v", code, m)
	}
	tok := m["access_token"].(string)
	if !f.en.validToken(tok) || f.en.validToken(tok+"x") || f.en.validToken("") {
		t.Fatal("issued token not accepted exactly")
	}
	if !tokenOK(tok, "", f.en) || tokenOK("nope", "", f.en) || tokenOK("", "", f.en) || !tokenOK("s", "s", f.en) {
		t.Fatal("tokenOK mismatch")
	}
	// The device code is single use.
	f.clk.advance(pollInterval)
	if code, m := f.poll(device); code != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("reuse: %d %v", code, m)
	}
}

func TestSlowDown(t *testing.T) {
	f := newFlow(t)
	device, _ := f.start()
	f.poll(device) // first poll is never too fast
	f.clk.advance(time.Second)
	if code, m := f.poll(device); code != 400 || m["error"] != "slow_down" {
		t.Fatalf("fast poll: %d %v", code, m)
	}
	// The interval grew by 5s: waiting the old interval is still too fast.
	f.clk.advance(pollInterval)
	if _, m := f.poll(device); m["error"] != "slow_down" {
		t.Fatalf("old interval: %v", m)
	}
	f.clk.advance(3 * pollInterval)
	if _, m := f.poll(device); m["error"] != "authorization_pending" {
		t.Fatalf("after waiting: %v", m)
	}
}

func TestDenied(t *testing.T) {
	f := newFlow(t)
	device, user := f.start()
	if code := f.activate(user, "deny"); code != 200 {
		t.Fatalf("activate: %d", code)
	}
	if code, m := f.poll(device); code != 400 || m["error"] != "access_denied" {
		t.Fatalf("denied: %d %v", code, m)
	}
}

func TestExpired(t *testing.T) {
	f := newFlow(t)
	device, user := f.start()
	f.clk.advance(codeLifetime + time.Second)
	if code, m := f.poll(device); code != 400 || m["error"] != "expired_token" {
		t.Fatalf("expired: %d %v", code, m)
	}
	if code := f.activate(user, "approve"); code != http.StatusNotFound {
		t.Fatalf("activating expired code: %d", code)
	}
}

func TestBadRequests(t *testing.T) {
	f := newFlow(t)
	if code, m := f.post("/enroll/device", url.Values{"client_id": {"other"}}); code != 400 || m["error"] != "invalid_client" {
		t.Fatalf("client: %d %v", code, m)
	}
	if code, m := f.post("/enroll/token", url.Values{"grant_type": {"password"}}); code != 400 || m["error"] != "unsupported_grant_type" {
		t.Fatalf("grant: %d %v", code, m)
	}
	if code, m := f.poll("unknown"); code != 400 || m["error"] != "invalid_grant" {
		t.Fatalf("unknown device: %d %v", code, m)
	}
	if code := f.activate("ZZZZ-ZZZZ", "approve"); code != http.StatusNotFound {
		t.Fatalf("unknown user code: %d", code)
	}
}
