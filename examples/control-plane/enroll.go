package main

// Enrollment: an OAuth 2.0 Device Authorization Grant (RFC 8628) server with
// ki-agent's discovery document, see "Enrollment" in docs/protocol.md.
// /activate stands in for a real login: whoever can reach it can approve.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html/template"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	grantDeviceCode = "urn:ietf:params:oauth:grant-type:device_code"
	// userCodeAlphabet has no vowels (no words) and no look-alikes (0/O, 1/I).
	userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ23456789"
	pollInterval     = 5 * time.Second
	codeLifetime     = 10 * time.Minute
)

type deviceStatus int

const (
	statusPending deviceStatus = iota
	statusApproved
	statusDenied
)

// device is one enrollment attempt.
type device struct {
	userCode, deviceCode string
	hostname, os, arch   string
	agentVersion         string
	status               deviceStatus
	expires              time.Time
	interval             time.Duration
	lastPoll             time.Time
	token                string // set once approved and delivered
}

// enroller holds the enrollment state, all in memory.
type enroller struct {
	publicURL string
	now       func() time.Time

	mu      sync.Mutex
	devices map[string]*device // by device code
	tokens  []string           // issued access tokens
}

func newEnroller(publicURL string, now func() time.Time) *enroller {
	return &enroller{publicURL: publicURL, now: now, devices: map[string]*device{}}
}

func (e *enroller) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/ki-agent-enrollment", e.discovery)
	mux.HandleFunc("POST /enroll/device", e.deviceAuthorization)
	mux.HandleFunc("POST /enroll/token", e.token)
	mux.HandleFunc("GET /activate", e.activateForm)
	mux.HandleFunc("POST /activate", e.activateSubmit)
}

// validToken reports whether t was issued by this enroller (constant time).
func (e *enroller) validToken(t string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	ok := false
	for _, have := range e.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(have)) == 1 {
			ok = true
		}
	}
	return ok
}

// base is the externally visible origin of this request.
func base(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]string{"error": code})
}

func (e *enroller) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{
		"device_authorization_endpoint": base(r) + "/enroll/device",
		"token_endpoint":                base(r) + "/enroll/token",
	})
}

func (e *enroller) deviceAuthorization(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request")
		return
	}
	if r.PostForm.Get("client_id") != "ki-agent" {
		oauthError(w, "invalid_client")
		return
	}
	d := &device{
		deviceCode:   randomToken(32),
		hostname:     r.PostForm.Get("hostname"),
		os:           r.PostForm.Get("os"),
		arch:         r.PostForm.Get("arch"),
		agentVersion: r.PostForm.Get("agent_version"),
		interval:     pollInterval,
		expires:      e.now().Add(codeLifetime),
	}
	e.mu.Lock()
	for { // user codes must be unique among live devices
		d.userCode = newUserCode()
		if e.byUserCode(d.userCode) == nil {
			break
		}
	}
	e.devices[d.deviceCode] = d
	e.mu.Unlock()
	log.Printf("enrollment requested by host=%q os=%s arch=%s agent=%s protocol=%s: code %s",
		d.hostname, d.os, d.arch, d.agentVersion, r.PostForm.Get("protocol"), formatUserCode(d.userCode))

	verify := base(r) + "/activate"
	writeJSON(w, http.StatusOK, map[string]any{
		"device_code":               d.deviceCode,
		"user_code":                 formatUserCode(d.userCode),
		"verification_uri":          verify,
		"verification_uri_complete": verify + "?user_code=" + url.QueryEscape(formatUserCode(d.userCode)),
		"expires_in":                int(codeLifetime / time.Second),
		"interval":                  int(pollInterval / time.Second),
	})
}

func (e *enroller) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		oauthError(w, "invalid_request")
		return
	}
	if r.PostForm.Get("grant_type") != grantDeviceCode {
		oauthError(w, "unsupported_grant_type")
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.devices[r.PostForm.Get("device_code")]
	if d == nil {
		oauthError(w, "invalid_grant")
		return
	}
	now := e.now()
	if !now.Before(d.expires) {
		delete(e.devices, d.deviceCode)
		oauthError(w, "expired_token")
		return
	}
	tooFast := !d.lastPoll.IsZero() && now.Sub(d.lastPoll) < d.interval
	d.lastPoll = now
	if tooFast {
		d.interval += 5 * time.Second
		oauthError(w, "slow_down")
		return
	}
	switch d.status {
	case statusDenied:
		delete(e.devices, d.deviceCode)
		oauthError(w, "access_denied")
	case statusApproved:
		delete(e.devices, d.deviceCode)
		tok := randomToken(32)
		e.tokens = append(e.tokens, tok)
		log.Printf("enrollment completed for host=%q (code %s)", d.hostname, formatUserCode(d.userCode))
		writeJSON(w, http.StatusOK, map[string]string{
			"access_token":      tok,
			"token_type":        "Bearer",
			"control_plane_url": e.publicURL,
		})
	default:
		oauthError(w, "authorization_pending")
	}
}

// byUserCode finds a live device by its user code; e.mu must be held.
func (e *enroller) byUserCode(code string) *device {
	code = normalizeUserCode(code)
	for _, d := range e.devices {
		if d.userCode == code && e.now().Before(d.expires) {
			return d
		}
	}
	return nil
}

// normalizeUserCode accepts any case and ignores dashes and spaces.
func normalizeUserCode(s string) string {
	s = strings.ToUpper(s)
	return strings.NewReplacer("-", "", " ", "").Replace(s)
}

// newUserCode returns 8 characters; the stored form has no dash (see
// normalizeUserCode) and formatUserCode shows it as XXXX-XXXX.
func newUserCode() string {
	b := make([]byte, 8)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(userCodeAlphabet))))
		if err != nil {
			panic(err)
		}
		b[i] = userCodeAlphabet[n.Int64()]
	}
	return string(b)
}

func formatUserCode(c string) string {
	if len(c) == 8 {
		return c[:4] + "-" + c[4:]
	}
	return c
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var activateTmpl = template.Must(template.New("activate").Parse(`<!doctype html>
<meta charset="utf-8"><title>Activate agent</title>
<body style="font-family:sans-serif;max-width:32em;margin:3em auto">
<h1>Activate agent</h1>
{{if .Message}}<p><strong>{{.Message}}</strong></p>{{end}}
{{if .Device}}<p>Agent <code>{{.Device.Host}}</code> ({{.Device.OS}}/{{.Device.Arch}}, ki-agent {{.Device.Version}}) wants to connect.</p>{{end}}
<form method="post" action="/activate">
<p><label>Code <input name="user_code" value="{{.Code}}" autocomplete="off" autofocus></label></p>
<p><button name="action" value="approve">Approve</button>
<button name="action" value="deny">Deny</button></p>
</form>
<p><small>Example only: a real control plane would sign the operator in first.</small></p>
</body>
`))

type activateView struct {
	Message, Code string
	Device        *struct{ Host, OS, Arch, Version string }
}

func (e *enroller) render(w http.ResponseWriter, status int, v activateView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = activateTmpl.Execute(w, v)
}

func (e *enroller) activateForm(w http.ResponseWriter, r *http.Request) {
	v := activateView{Code: r.URL.Query().Get("user_code")}
	e.mu.Lock()
	if d := e.byUserCode(v.Code); d != nil {
		v.Code = formatUserCode(d.userCode)
		v.Device = &struct{ Host, OS, Arch, Version string }{d.hostname, d.os, d.arch, d.agentVersion}
	}
	e.mu.Unlock()
	e.render(w, http.StatusOK, v)
}

func (e *enroller) activateSubmit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		e.render(w, http.StatusBadRequest, activateView{Message: "Bad request."})
		return
	}
	code, action := r.PostForm.Get("user_code"), r.PostForm.Get("action")
	e.mu.Lock()
	d := e.byUserCode(code)
	var msg, verdict string
	status := http.StatusOK
	switch {
	case d == nil:
		msg, status = "Unknown or expired code.", http.StatusNotFound
	case d.status != statusPending:
		msg, status = "This code was already used.", http.StatusConflict
	case action == "approve":
		d.status = statusApproved
		msg, verdict = "Approved. The agent will connect shortly.", "approved"
	case action == "deny":
		d.status = statusDenied
		msg, verdict = "Denied.", "denied"
	default:
		msg, status = "Choose approve or deny.", http.StatusBadRequest
	}
	e.mu.Unlock()
	if verdict != "" {
		log.Printf("enrollment %s: code %s", verdict, formatUserCode(normalizeUserCode(code)))
	}
	e.render(w, status, activateView{Message: msg})
}
