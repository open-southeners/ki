package panel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/accounts"
	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/server"
)

type stub struct {
	loginErr, finishErr, beginErr error
	loggedOut                     string
	lastLogin                     accounts.LoginRequest
	lastFinish                    accounts.SetupFinishRequest
}

var user = auth.Principal{Kind: auth.KindUser, ID: 1, Name: "ann", Role: auth.RoleAdmin}

func (s *stub) Login(_ context.Context, r accounts.LoginRequest) (string, auth.Principal, error) {
	s.lastLogin = r
	if s.loginErr != nil {
		return "", auth.Principal{}, s.loginErr
	}
	return "sid1", user, nil
}

func (s *stub) Logout(_ context.Context, id string) error { s.loggedOut = id; return nil }

func (s *stub) Session(_ context.Context, id string) (auth.Principal, error) {
	if id == "sid1" {
		return user, nil
	}
	return auth.Principal{}, accounts.ErrUnauthenticated
}

func (s *stub) SetupBegin(_ context.Context, tok string) (*accounts.SetupInfo, error) {
	if s.beginErr != nil {
		return nil, s.beginErr
	}
	return &accounts.SetupInfo{Username: "ann", Role: "admin", TOTPSecret: "SEC", OTPAuthURI: "otpauth://x"}, nil
}

func (s *stub) SetupFinish(_ context.Context, r accounts.SetupFinishRequest) (string, auth.Principal, error) {
	s.lastFinish = r
	if s.finishErr != nil {
		return "", auth.Principal{}, s.finishErr
	}
	return "sid1", user, nil
}

func newH(s *stub, max time.Duration) http.Handler {
	return New(Options{Accounts: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), SessionMax: max})
}

func do(h http.Handler, method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "panel.test"
	r.RemoteAddr = "10.0.0.9:5555"
	if method == http.MethodPost {
		r.Header.Set("Origin", "https://panel.test")
		r.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestLogin(t *testing.T) {
	s := &stub{}
	w := do(newH(s, 0), "POST", "/auth/login", `{"username":"ann","password":"pw","code":"123456"}`, nil)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"role":"admin","username":"ann"}` {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if s.lastLogin.ClientIP != "10.0.0.9" || s.lastLogin.Username != "ann" {
		t.Fatalf("login req = %+v", s.lastLogin)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].Name != server.SessionCookie || c[0].Value != "sid1" {
		t.Fatalf("cookies = %v", c)
	}
	if !c[0].Secure || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Path != "/" || c[0].Domain != "" || c[0].MaxAge != 0 {
		t.Fatalf("cookie attrs: %+v", c[0])
	}
}

func TestCookieMaxAge(t *testing.T) {
	w := do(newH(&stub{}, 2*time.Hour), "POST", "/auth/login", `{"username":"a","password":"b","code":"c"}`, nil)
	if c := w.Result().Cookies(); len(c) != 1 || c[0].MaxAge != 7200 {
		t.Fatalf("cookies = %v", c)
	}
}

func TestLoginErrors(t *testing.T) {
	tests := []struct {
		name  string
		err   error
		code  int
		retry string
	}{
		{"invalid", accounts.ErrInvalidCredentials, 401, ""},
		{"rate limited", &accounts.RateLimitedError{RetryAfter: 90*time.Second + time.Millisecond}, 429, "91"},
		{"busy", auth.ErrBusy, 503, "1"},
		{"other", errors.New("db down"), 500, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(newH(&stub{loginErr: tt.err}, 0), "POST", "/auth/login", `{"username":"a","password":"b","code":"c"}`, nil)
			if w.Code != tt.code || w.Header().Get("Retry-After") != tt.retry {
				t.Fatalf("got %d retry=%q", w.Code, w.Header().Get("Retry-After"))
			}
			if len(w.Result().Cookies()) != 0 {
				t.Fatal("cookie set on failure")
			}
			if tt.code == 401 && !strings.Contains(w.Body.String(), `"invalid credentials"`) {
				t.Fatalf("body = %s", w.Body)
			}
		})
	}
}

func TestSetupBegin(t *testing.T) {
	w := do(newH(&stub{}, 0), "POST", "/auth/setup/begin", `{"token":"t"}`, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"totp_secret":"SEC"`) || !strings.Contains(w.Body.String(), `"otpauth_uri":"otpauth://x"`) {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	for _, e := range []error{accounts.ErrInvalidSetup, errors.New("boom")} {
		w = do(newH(&stub{beginErr: e}, 0), "POST", "/auth/setup/begin", `{"token":"t"}`, nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid or expired setup link") {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	}
}

func TestSetupFinish(t *testing.T) {
	body := `{"token":"t","password":"pw","code":"123456"}`
	s := &stub{}
	w := do(newH(s, 0), "POST", "/auth/setup/finish", body, map[string]string{"User-Agent": "ua"})
	if w.Code != 200 || len(w.Result().Cookies()) != 1 || s.lastFinish.Token != "t" || s.lastFinish.UserAgent != "ua" {
		t.Fatalf("got %d %s %+v", w.Code, w.Body, s.lastFinish)
	}
	weak := &stub{finishErr: errors.Join(accounts.ErrWeakPassword)}
	w = do(newH(weak, 0), "POST", "/auth/setup/finish", body, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), accounts.ErrWeakPassword.Error()) {
		t.Fatalf("weak: %d %s", w.Code, w.Body)
	}
	rl := &stub{finishErr: &accounts.RateLimitedError{RetryAfter: time.Second}}
	w = do(newH(rl, 0), "POST", "/auth/setup/finish", body, nil)
	if w.Code != 429 || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("rl: %d %v", w.Code, w.Header())
	}
	for _, e := range []error{accounts.ErrInvalidSetup, accounts.ErrInvalidCredentials, errors.New("x")} {
		w = do(newH(&stub{finishErr: e}, 0), "POST", "/auth/setup/finish", body, nil)
		if w.Code != 400 {
			t.Fatalf("%v: %d", e, w.Code)
		}
	}
}

func TestLogout(t *testing.T) {
	s := &stub{}
	w := do(newH(s, 0), "POST", "/auth/logout", `{}`, map[string]string{"Cookie": server.SessionCookie + "=sid1"})
	if w.Code != 204 || s.loggedOut != "sid1" {
		t.Fatalf("got %d, loggedOut=%q", w.Code, s.loggedOut)
	}
	c := w.Result().Cookies()
	if len(c) != 1 || c[0].MaxAge != -1 || c[0].Value != "" || !c[0].Secure || !c[0].HttpOnly {
		t.Fatalf("cookies = %+v", c)
	}
	s = &stub{}
	w = do(newH(s, 0), "POST", "/auth/logout", `{}`, nil)
	if w.Code != 204 || s.loggedOut != "" {
		t.Fatalf("no cookie: %d %q", w.Code, s.loggedOut)
	}
}

func TestSession(t *testing.T) {
	h := newH(&stub{}, 0)
	w := do(h, "GET", "/auth/session", "", map[string]string{"Cookie": server.SessionCookie + "=sid1"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ann"`) {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if w = do(h, "GET", "/auth/session", "", map[string]string{"Cookie": server.SessionCookie + "=bad"}); w.Code != 401 {
		t.Fatalf("bad: %d", w.Code)
	}
	if w = do(h, "GET", "/auth/session", "", nil); w.Code != 401 {
		t.Fatalf("none: %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("session response cacheable")
	}
}

func TestCSRFAndBody(t *testing.T) {
	h := newH(&stub{}, 0)
	const ok = `{"username":"a","password":"b","code":"c"}`
	tests := []struct {
		name string
		body string
		hdr  map[string]string
		code int
	}{
		{"missing origin", ok, map[string]string{"Origin": ""}, 403},
		{"cross origin", ok, map[string]string{"Origin": "https://evil.test"}, 403},
		{"null origin", ok, map[string]string{"Origin": "null"}, 403},
		{"other port", ok, map[string]string{"Origin": "https://panel.test:8443"}, 403},
		{"form content type", ok, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415},
		{"no content type", ok, map[string]string{"Content-Type": ""}, 415},
		{"json with charset", ok, map[string]string{"Content-Type": "application/json; charset=utf-8"}, 200},
		{"unknown field", `{"username":"a","password":"b","code":"c","x":1}`, nil, 400},
		{"bad json", `{`, nil, 400},
		{"trailing data", ok + ok, nil, 400},
		{"too large", `{"username":"` + strings.Repeat("a", maxBody) + `"}`, nil, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if w := do(h, "POST", "/auth/login", tt.body, tt.hdr); w.Code != tt.code {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body, tt.code)
			}
		})
	}
	// Every POST endpoint is protected, and only POST is accepted.
	for _, p := range []string{"/auth/setup/begin", "/auth/setup/finish", "/auth/logout"} {
		if w := do(h, "POST", p, `{}`, map[string]string{"Origin": ""}); w.Code != 403 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	if w := do(h, "GET", "/auth/login", "", nil); w.Code != 405 {
		t.Fatalf("GET login: %d", w.Code)
	}
}

func TestClientIPFromEdge(t *testing.T) {
	s := &stub{}
	h := newH(s, 0)
	body := `{"username":"a","password":"b","code":"c"}`
	r := httptest.NewRequest("POST", "/auth/login", strings.NewReader(body))
	r.Host, r.RemoteAddr = "panel.test", "127.0.0.1:1"
	r.Header.Set("Origin", "https://panel.test")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.7")

	h.ServeHTTP(httptest.NewRecorder(), r)
	if s.lastLogin.ClientIP != "127.0.0.1" {
		t.Fatalf("direct ip = %q (XFF must be ignored)", s.lastLogin.ClientIP)
	}
	r2 := r.Clone(server.WithEdge(r.Context(), true))
	r2.Body = io.NopCloser(strings.NewReader(body))
	h.ServeHTTP(httptest.NewRecorder(), r2)
	if s.lastLogin.ClientIP != "203.0.113.7" {
		t.Fatalf("edge ip = %q, want the last X-Forwarded-For hop", s.lastLogin.ClientIP)
	}
}

func TestPageAndHeaders(t *testing.T) {
	h := newH(&stub{}, 0)
	for _, p := range []string{"/", "/setup"} {
		w := do(h, "GET", p, "", nil)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(w.Body.String(), "<form") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Header().Get("Content-Type"))
		}
	}
	for _, p := range []string{"/app.js", "/app.css"} {
		if w := do(h, "GET", p, "", nil); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, w.Code)
		}
	}
	w := do(h, "GET", "/nope", "", nil)
	if w.Code != 404 || !strings.Contains(w.Header().Get("Content-Type"), "json") {
		t.Fatalf("404: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	for _, p := range []string{"/", "/nope", "/auth/session"} {
		w := do(h, "GET", p, "", nil)
		want := map[string]string{
			"Content-Security-Policy": "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "no-referrer",
			"X-Frame-Options":         "DENY",
		}
		for k, v := range want {
			if w.Header().Get(k) != v {
				t.Fatalf("%s %s = %q", p, k, w.Header().Get(k))
			}
		}
	}
	if w := do(h, "POST", "/auth/login", `{}`, map[string]string{"Origin": ""}); w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatal("error responses lack headers")
	}
}

func TestPageSelfContained(t *testing.T) {
	for _, n := range []string{"index.html", "app.js", "app.css"} {
		b, err := staticFS.ReadFile("static/" + n)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("http://")) || bytes.Contains(b, []byte("https://")) {
			t.Fatalf("%s references an external resource", n)
		}
	}
	b, _ := staticFS.ReadFile("static/index.html")
	if bytes.Contains(b, []byte("onclick=")) || bytes.Contains(b, []byte("<style")) {
		t.Fatal("inline script or style would break the CSP")
	}
}
