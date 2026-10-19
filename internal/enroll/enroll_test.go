package enroll

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testDeviceCode = "dev-code-SECRET"
	testToken      = "tok-SECRET"
)

// fakeServer is a device-flow server whose token responses are scripted.
type fakeServer struct {
	srv *httptest.Server

	mu         sync.Mutex
	tokenCalls int
	tokenResp  func(n int, w http.ResponseWriter)
	discovery  func(w http.ResponseWriter, base string)
	device     func(w http.ResponseWriter)
	lastDevice map[string]string
	lastHeader http.Header
	lastToken  map[string]string
}

func newFake(t *testing.T, tlsServer bool) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/ki-agent-enrollment", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastHeader = r.Header.Clone()
		f.mu.Unlock()
		if f.discovery != nil {
			f.discovery(w, f.srv.URL)
			return
		}
		writeJSON(w, 200, `{"device_authorization_endpoint":"`+f.srv.URL+`/device","token_endpoint":"`+f.srv.URL+`/token"}`)
	})
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.lastDevice = map[string]string{"ct": r.Header.Get("Content-Type"), "method": r.Method}
		for k := range r.PostForm {
			f.lastDevice[k] = r.PostForm.Get(k)
		}
		f.mu.Unlock()
		if f.device != nil {
			f.device(w)
			return
		}
		writeJSON(w, 200, `{"device_code":"`+testDeviceCode+`","user_code":"ABCD-1234","verification_uri":"https://example.test/activate","verification_uri_complete":"https://example.test/activate?c=ABCD-1234","expires_in":600,"interval":2}`)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokenCalls++
		n := f.tokenCalls
		f.lastToken = map[string]string{}
		for k := range r.PostForm {
			f.lastToken[k] = r.PostForm.Get(k)
		}
		f.mu.Unlock()
		f.tokenResp(n, w)
	})
	if tlsServer {
		f.srv = httptest.NewTLSServer(mux)
	} else {
		f.srv = httptest.NewServer(mux)
	}
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

func success(w http.ResponseWriter) {
	writeJSON(w, 200, `{"access_token":"`+testToken+`","token_type":"Bearer","control_plane_url":"wss://cp.example.test/agent"}`)
}

// clock is a fake time source advanced by the Sleep hook.
type clock struct {
	mu     sync.Mutex
	t      time.Time
	sleeps []time.Duration
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sleeps = append(c.sleeps, d)
	c.t = c.t.Add(d)
	return nil
}

func newClient(f *fakeServer, clk *clock) *Client {
	opts := Options{UserAgent: "ki-agent/test", Now: clk.now, Sleep: clk.sleep}
	if f.srv.TLS != nil {
		pool := x509.NewCertPool()
		pool.AddCert(f.srv.Certificate())
		opts.TLSConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return NewClient(opts)
}

func newClock() *clock { return &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)} }

func req(base string) Request {
	return Request{BaseURL: base, Hostname: "h1", OS: "linux", Arch: "amd64", AgentVersion: "1.2.3", Protocol: "1.0.0"}
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var ee *Error
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *Error", err)
	}
	if ee.Reason != reason {
		t.Fatalf("reason = %q (%v), want %q", ee.Reason, err, reason)
	}
	for _, secret := range []string{testDeviceCode, testToken} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks a secret: %v", err)
		}
	}
}

func TestHappyPathTLS(t *testing.T) {
	f := newFake(t, true)
	f.tokenResp = func(_ int, w http.ResponseWriter) { success(w) }
	clk := newClock()
	c := newClient(f, clk)

	p, err := c.Start(context.Background(), req(f.srv.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	if p.UserCode != "ABCD-1234" || p.VerificationURI != "https://example.test/activate" ||
		p.VerificationURIComplete != "https://example.test/activate?c=ABCD-1234" {
		t.Fatalf("pending = %+v", p)
	}
	if want := clk.now().Add(600 * time.Second); !p.ExpiresAt.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", p.ExpiresAt, want)
	}
	for k, want := range map[string]string{
		"method": "POST", "ct": "application/x-www-form-urlencoded", "client_id": "ki-agent",
		"hostname": "h1", "os": "linux", "arch": "amd64", "agent_version": "1.2.3", "protocol": "1.0.0",
	} {
		if f.lastDevice[k] != want {
			t.Errorf("device form %s = %q, want %q", k, f.lastDevice[k], want)
		}
	}
	if f.lastHeader.Get("User-Agent") != "ki-agent/test" || f.lastHeader.Get("Accept") != "application/json" {
		t.Errorf("headers = %v", f.lastHeader)
	}

	cred, err := c.Wait(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "wss://cp.example.test/agent" || cred.Token != testToken ||
		cred.EnrollmentURL != f.srv.URL+"/" || !cred.EnrolledAt.Equal(clk.now()) {
		t.Fatalf("cred = %+v", cred)
	}
	if f.lastToken["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" ||
		f.lastToken["device_code"] != testDeviceCode || f.lastToken["client_id"] != "ki-agent" {
		t.Errorf("token form = %v", f.lastToken)
	}
	if len(clk.sleeps) != 1 || clk.sleeps[0] != 2*time.Second {
		t.Errorf("sleeps = %v", clk.sleeps)
	}
}

func TestLoopbackHTTPAndDefaults(t *testing.T) {
	f := newFake(t, false)
	f.device = func(w http.ResponseWriter) {
		writeJSON(w, 200, `{"device_code":"d","user_code":"U","verification_uri":"http://127.0.0.1/a","expires_in":99999}`)
	}
	f.tokenResp = func(_ int, w http.ResponseWriter) { success(w) }
	clk := newClock()
	c := newClient(f, clk)
	p, err := c.Start(context.Background(), req(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if want := clk.now().Add(30 * time.Minute); !p.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want capped at %v", p.ExpiresAt, want)
	}
	if _, err := c.Wait(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if clk.sleeps[0] != 5*time.Second {
		t.Errorf("default interval = %v, want 5s", clk.sleeps[0])
	}
}

func TestPollingStates(t *testing.T) {
	tests := []struct {
		name       string
		resp       func(n int, w http.ResponseWriter)
		wantReason string
		wantSleeps []time.Duration
		wantCalls  int
	}{
		{
			name: "pending then success",
			resp: func(n int, w http.ResponseWriter) {
				if n < 3 {
					writeJSON(w, 400, `{"error":"authorization_pending"}`)
					return
				}
				success(w)
			},
			wantSleeps: []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second},
			wantCalls:  3,
		},
		{
			name: "slow_down raises interval",
			resp: func(n int, w http.ResponseWriter) {
				switch n {
				case 1:
					writeJSON(w, 400, `{"error":"slow_down"}`)
				case 2:
					writeJSON(w, 400, `{"error":"slow_down"}`)
				default:
					success(w)
				}
			},
			wantSleeps: []time.Duration{2 * time.Second, 7 * time.Second, 12 * time.Second},
			wantCalls:  3,
		},
		{
			name:       "denied",
			resp:       func(_ int, w http.ResponseWriter) { writeJSON(w, 400, `{"error":"access_denied"}`) },
			wantReason: ReasonDenied,
			wantCalls:  1,
		},
		{
			name:       "expired",
			resp:       func(_ int, w http.ResponseWriter) { writeJSON(w, 400, `{"error":"expired_token"}`) },
			wantReason: ReasonExpired,
			wantCalls:  1,
		},
		{
			name:       "unknown error code",
			resp:       func(_ int, w http.ResponseWriter) { writeJSON(w, 400, `{"error":"invalid_grant"}`) },
			wantReason: ReasonFailed,
			wantCalls:  1,
		},
		{
			name:       "400 without code",
			resp:       func(_ int, w http.ResponseWriter) { writeJSON(w, 400, `{}`) },
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
		{
			name:       "unexpected status",
			resp:       func(_ int, w http.ResponseWriter) { writeJSON(w, 403, `{}`) },
			wantReason: ReasonFailed,
			wantCalls:  1,
		},
		{
			name: "transient 5xx recovers",
			resp: func(n int, w http.ResponseWriter) {
				if n < 3 {
					http.Error(w, "boom", 503)
					return
				}
				success(w)
			},
			wantCalls: 3,
		},
		{
			name:       "persistent 5xx",
			resp:       func(_ int, w http.ResponseWriter) { http.Error(w, "boom", 502) },
			wantReason: ReasonUnreachable,
			wantCalls:  maxTransient,
		},
		{
			name: "oversized body",
			resp: func(_ int, w http.ResponseWriter) {
				writeJSON(w, 200, `{"access_token":"`+strings.Repeat("a", 70<<10)+`"}`)
			},
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
		{
			name:       "bad JSON",
			resp:       func(_ int, w http.ResponseWriter) { writeJSON(w, 200, `{"access_token":`) },
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
		{
			name: "missing token",
			resp: func(_ int, w http.ResponseWriter) {
				writeJSON(w, 200, `{"token_type":"Bearer","control_plane_url":"wss://cp.example.test/a"}`)
			},
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
		{
			name: "wrong token type",
			resp: func(_ int, w http.ResponseWriter) {
				writeJSON(w, 200, `{"access_token":"`+testToken+`","token_type":"mac","control_plane_url":"wss://cp.example.test/a"}`)
			},
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
		{
			name: "ws to non-loopback",
			resp: func(_ int, w http.ResponseWriter) {
				writeJSON(w, 200, `{"access_token":"`+testToken+`","token_type":"Bearer","control_plane_url":"ws://cp.example.test/a"}`)
			},
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
		{
			name: "https control plane url",
			resp: func(_ int, w http.ResponseWriter) {
				writeJSON(w, 200, `{"access_token":"`+testToken+`","token_type":"Bearer","control_plane_url":"https://cp.example.test/a"}`)
			},
			wantReason: ReasonInvalidResponse,
			wantCalls:  1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, true)
			f.tokenResp = tt.resp
			clk := newClock()
			c := newClient(f, clk)
			p, err := c.Start(context.Background(), req(f.srv.URL))
			if err != nil {
				t.Fatal(err)
			}
			cred, err := c.Wait(context.Background(), p)
			if tt.wantReason == "" {
				if err != nil {
					t.Fatal(err)
				}
				if cred.Token != testToken {
					t.Errorf("cred = %+v", cred)
				}
			} else {
				wantReason(t, err, tt.wantReason)
				if cred != (Credentials{}) {
					t.Errorf("cred = %+v, want zero", cred)
				}
			}
			if f.tokenCalls != tt.wantCalls {
				t.Errorf("token calls = %d, want %d", f.tokenCalls, tt.wantCalls)
			}
			if tt.wantSleeps != nil {
				if len(clk.sleeps) != len(tt.wantSleeps) {
					t.Fatalf("sleeps = %v, want %v", clk.sleeps, tt.wantSleeps)
				}
				for i := range tt.wantSleeps {
					if clk.sleeps[i] != tt.wantSleeps[i] {
						t.Fatalf("sleeps = %v, want %v", clk.sleeps, tt.wantSleeps)
					}
				}
			}
		})
	}
}

func TestWaitDeadline(t *testing.T) {
	f := newFake(t, true)
	f.device = func(w http.ResponseWriter) {
		writeJSON(w, 200, `{"device_code":"`+testDeviceCode+`","user_code":"U","verification_uri":"https://example.test/a","expires_in":10,"interval":3}`)
	}
	f.tokenResp = func(_ int, w http.ResponseWriter) { writeJSON(w, 400, `{"error":"authorization_pending"}`) }
	clk := newClock()
	c := newClient(f, clk)
	p, err := c.Start(context.Background(), req(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Wait(context.Background(), p)
	wantReason(t, err, ReasonExpired)
	if f.tokenCalls != 3 { // polls at t=3,6,9; the sleep to t=12 ends the flow
		t.Errorf("token calls = %d, want 3", f.tokenCalls)
	}
}

func TestWaitContextCancelled(t *testing.T) {
	f := newFake(t, true)
	f.tokenResp = func(_ int, w http.ResponseWriter) { success(w) }
	c := newClient(f, newClock())
	p, err := c.Start(context.Background(), req(f.srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Wait(ctx, p); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if f.tokenCalls != 0 {
		t.Errorf("token calls = %d, want 0", f.tokenCalls)
	}
}

func TestWaitNilPending(t *testing.T) {
	_, err := NewClient(Options{}).Wait(context.Background(), nil)
	wantReason(t, err, ReasonFailed)
}

func TestStartErrors(t *testing.T) {
	tests := []struct {
		name       string
		base       func(f *fakeServer) string
		discovery  func(w http.ResponseWriter, base string)
		device     func(w http.ResponseWriter)
		wantReason string
	}{
		{name: "empty base", base: func(*fakeServer) string { return "" }, wantReason: ReasonFailed},
		{name: "http non-loopback base", base: func(*fakeServer) string { return "http://example.com" }, wantReason: ReasonFailed},
		{name: "ftp base", base: func(*fakeServer) string { return "ftp://example.com" }, wantReason: ReasonFailed},
		{name: "no host", base: func(*fakeServer) string { return "https:///x" }, wantReason: ReasonFailed},
		{name: "credentials in base", base: func(*fakeServer) string { return "https://u:p@example.com" }, wantReason: ReasonFailed},
		{
			name:       "discovery not found",
			discovery:  func(w http.ResponseWriter, _ string) { writeJSON(w, 404, `{}`) },
			wantReason: ReasonInvalidResponse,
		},
		{
			name:       "discovery bad JSON",
			discovery:  func(w http.ResponseWriter, _ string) { writeJSON(w, 200, `nope`) },
			wantReason: ReasonInvalidResponse,
		},
		{
			name:       "discovery oversized",
			discovery:  func(w http.ResponseWriter, _ string) { writeJSON(w, 200, `{"x":"`+strings.Repeat("a", 65<<10)+`"}`) },
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "discovery http device endpoint",
			discovery: func(w http.ResponseWriter, base string) {
				writeJSON(w, 200, `{"device_authorization_endpoint":"http://example.com/d","token_endpoint":"`+base+`/token"}`)
			},
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "discovery http token endpoint",
			discovery: func(w http.ResponseWriter, base string) {
				writeJSON(w, 200, `{"device_authorization_endpoint":"`+base+`/device","token_endpoint":"http://example.com/t"}`)
			},
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "discovery missing endpoint",
			discovery: func(w http.ResponseWriter, base string) {
				writeJSON(w, 200, `{"device_authorization_endpoint":"`+base+`/device"}`)
			},
			wantReason: ReasonInvalidResponse,
		},
		{
			name:       "device authorization error status",
			device:     func(w http.ResponseWriter) { writeJSON(w, 400, `{"error":"invalid_request"}`) },
			wantReason: ReasonFailed,
		},
		{
			name:       "device missing fields",
			device:     func(w http.ResponseWriter) { writeJSON(w, 200, `{"user_code":"U"}`) },
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "device bad expires_in",
			device: func(w http.ResponseWriter) {
				writeJSON(w, 200, `{"device_code":"d","user_code":"U","verification_uri":"https://e.test/a","expires_in":0}`)
			},
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "device non-http verification uri",
			device: func(w http.ResponseWriter) {
				writeJSON(w, 200, `{"device_code":"d","user_code":"U","verification_uri":"javascript:alert(1)","expires_in":60}`)
			},
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "device control chars in user code",
			device: func(w http.ResponseWriter) {
				writeJSON(w, 200, `{"device_code":"d","user_code":"\u001b[31mU","verification_uri":"https://e.test/a","expires_in":60}`)
			},
			wantReason: ReasonInvalidResponse,
		},
		{
			name: "device bad complete uri",
			device: func(w http.ResponseWriter) {
				writeJSON(w, 200, `{"device_code":"d","user_code":"U","verification_uri":"https://e.test/a","verification_uri_complete":"file:///etc/passwd","expires_in":60}`)
			},
			wantReason: ReasonInvalidResponse,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t, true)
			f.discovery, f.device = tt.discovery, tt.device
			base := f.srv.URL
			if tt.base != nil {
				base = tt.base(f)
			}
			_, err := newClient(f, newClock()).Start(context.Background(), req(base))
			wantReason(t, err, tt.wantReason)
		})
	}
}

func TestStartUnreachable(t *testing.T) {
	f := newFake(t, false)
	base := f.srv.URL
	f.srv.Close()
	_, err := NewClient(Options{}).Start(context.Background(), req(base))
	wantReason(t, err, ReasonUnreachable)
}

func TestStartUntrustedCertificate(t *testing.T) {
	f := newFake(t, true)
	_, err := NewClient(Options{}).Start(context.Background(), req(f.srv.URL))
	wantReason(t, err, ReasonUnreachable)
}

func TestRedirectToInsecureRefused(t *testing.T) {
	f := newFake(t, false)
	f.discovery = func(w http.ResponseWriter, _ string) {
		w.Header().Set("Location", "http://example.com/x")
		w.WriteHeader(http.StatusFound)
	}
	_, err := newClient(f, newClock()).Start(context.Background(), req(f.srv.URL))
	wantReason(t, err, ReasonUnreachable)
}
