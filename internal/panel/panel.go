// Package panel serves the web panel: the /auth JSON endpoints (setup, login,
// logout, session) and the embedded single-page UI. The WebSocket /rpc
// endpoint is served by internal/server, which mounts this handler on both of
// its listeners.
package panel

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/open-southeners/ki-agent/internal/accounts"
	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/server"
)

// staticFS is the minimal built-in page, served when the web app is not built.
//
//go:embed static
var staticFS embed.FS

// distFS holds the built web app (web/, `bun run build`). Only the committed
// placeholders are present in a checkout that has not built it.
//
//go:embed dist
var distFS embed.FS

const (
	maxBody = 16 << 10
	// cspStatic covers the minimal built-in page.
	cspStatic = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	// cspSPA covers the web app. style-src allows inline styles only because
	// Radix/shadcn position popovers with style attributes; scripts are never
	// inline. data: images are for the TOTP QR code.
	cspSPA = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
)

// Accounts is the part of accounts.Service the panel needs.
type Accounts interface {
	Login(ctx context.Context, req accounts.LoginRequest) (string, auth.Principal, error)
	Logout(ctx context.Context, sessionID string) error
	Session(ctx context.Context, sessionID string) (auth.Principal, error)
	SetupBegin(ctx context.Context, token string) (*accounts.SetupInfo, error)
	SetupFinish(ctx context.Context, req accounts.SetupFinishRequest) (string, auth.Principal, error)
}

// Options configures the panel handler.
type Options struct {
	Accounts Accounts
	Logger   *slog.Logger
	// SessionMax, when positive, is the Max-Age of the session cookie. When
	// zero the cookie is a browser-session cookie (the server still enforces
	// the idle and absolute session limits).
	SessionMax time.Duration
	// Assets is the built web app (index.html at its root). When nil the
	// embedded build is used; when it has no index.html the minimal built-in
	// page is served instead. Tests inject their own.
	Assets fs.FS
}

type handler struct {
	acc  Accounts
	log  *slog.Logger
	max  time.Duration
	page map[string][]byte
	// assets is non-nil in SPA mode.
	assets fs.FS
	csp    string
}

// New returns the panel http.Handler.
func New(o Options) http.Handler {
	h := &handler{acc: o.Accounts, log: o.Logger, max: o.SessionMax, page: map[string][]byte{}, csp: cspStatic}
	if h.log == nil {
		h.log = slog.Default()
	}
	assets := o.Assets
	if assets == nil {
		if sub, err := fs.Sub(distFS, "dist"); err == nil {
			assets = sub
		}
	}
	if assets != nil {
		if st, err := fs.Stat(assets, "index.html"); err == nil && !st.IsDir() {
			h.assets, h.csp = assets, cspSPA
		}
	}
	for _, n := range []string{"index.html", "app.js", "app.css"} {
		b, err := staticFS.ReadFile("static/" + n)
		if err != nil {
			panic("panel: missing embedded " + n)
		}
		h.page[n] = b
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/setup/begin", h.post(h.setupBegin))
	mux.HandleFunc("/auth/setup/finish", h.post(h.setupFinish))
	mux.HandleFunc("/auth/login", h.post(h.login))
	mux.HandleFunc("/auth/logout", h.post(h.logout))
	mux.HandleFunc("/auth/session", h.session)
	if h.assets != nil {
		mux.HandleFunc("/", h.spa)
	} else {
		mux.HandleFunc("/", h.static)
	}
	return secure(mux, h.csp)
}

// secure sets the headers every response carries.
func secure(next http.Handler, csp string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", csp)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("X-Frame-Options", "DENY")
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			hd.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (h *handler) static(w http.ResponseWriter, r *http.Request) {
	var name, ctype string
	switch r.URL.Path {
	case "/", "/setup":
		name, ctype = "index.html", "text/html; charset=utf-8"
	case "/app.js":
		name, ctype = "app.js", "text/javascript; charset=utf-8"
	case "/app.css":
		name, ctype = "app.css", "text/css; charset=utf-8"
	default:
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(h.page[name])
}

// spa serves the built web app: real files by path, index.html for every
// other route-like path (client-side routing). Paths owned by other handlers
// (/auth/*, /rpc, /hooks/*) and unknown asset paths are 404.
func (h *handler) spa(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	p := path.Clean("/" + r.URL.Path)
	if reserved(p) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	name := strings.TrimPrefix(p, "/")
	if name != "" && name != "index.html" {
		if !fs.ValidPath(name) || hiddenAsset(name) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if st, err := fs.Stat(h.assets, name); err == nil && !st.IsDir() {
			h.serveFile(w, r, name)
			return
		}
		// Anything that looks like a file is a missing asset, not a route.
		if name == "assets" || strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
	}
	h.serveFile(w, r, "index.html")
}

// reserved reports paths served by other handlers; the app never answers them.
func reserved(p string) bool {
	for _, r := range []string{"/rpc", "/auth", "/hooks"} {
		if p == r || strings.HasPrefix(p, r+"/") {
			return true
		}
	}
	return false
}

// hiddenAsset reports files in the dist directory that are not part of the app.
func hiddenAsset(name string) bool {
	return name == "README.md" || name == ".keep"
}

func (h *handler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	b, err := fs.ReadFile(h.assets, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", contentType(name))
	if strings.HasPrefix(name, "assets/") {
		// Vite names these by content hash.
		hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		hd.Set("Cache-Control", "no-cache")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

func contentType(name string) string {
	switch path.Ext(name) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	case ".json", ".webmanifest":
		return "application/json"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// post wraps a POST endpoint with the CSRF checks: a same-host Origin header
// (required) and a JSON content type.
func (h *handler) post(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		o := r.Header.Get("Origin")
		u, err := url.Parse(o)
		if o == "" || err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		ct := r.Header.Get("Content-Type")
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
			writeErr(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	}
}

// decode reads a single strict JSON object into v.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, err2 := dec.Token(); err2 != io.EOF {
			err = errors.New("trailing data")
		}
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid request")
		}
		return false
	}
	return true
}

func clientIP(r *http.Request) string { return auth.ClientIP(r, server.FromEdge(r.Context())) }

func setCookie(w http.ResponseWriter, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name: server.SessionCookie, Value: id, Path: "/", MaxAge: maxAge,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
}

func (h *handler) startSession(w http.ResponseWriter, id string, p auth.Principal) {
	age := 0
	if h.max > 0 {
		age = int(h.max / time.Second)
	}
	setCookie(w, id, age)
	writeJSON(w, http.StatusOK, map[string]string{"username": p.Name, "role": p.Role.String()})
}

// authFail maps the errors shared by login and setup completion.
func (h *handler) authFail(w http.ResponseWriter, err error, op string) {
	var rl *accounts.RateLimitedError
	switch {
	case errors.As(err, &rl):
		secs := int((rl.RetryAfter + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusServiceUnavailable, "server busy, try again")
	case errors.Is(err, accounts.ErrWeakPassword):
		writeErr(w, http.StatusBadRequest, err.Error())
	case op == "login" && errors.Is(err, accounts.ErrInvalidCredentials):
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
	case op == "login":
		h.log.Error("panel login", "err", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	case errors.Is(err, accounts.ErrInvalidSetup), errors.Is(err, accounts.ErrInvalidCredentials):
		writeErr(w, http.StatusBadRequest, "invalid or expired setup link")
	default:
		h.log.Warn("panel setup", "op", op, "err", err)
		writeErr(w, http.StatusBadRequest, "setup failed")
	}
}

func (h *handler) setupBegin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &in) {
		return
	}
	info, err := h.acc.SetupBegin(r.Context(), in.Token)
	if err != nil {
		if !errors.Is(err, accounts.ErrInvalidSetup) {
			h.log.Warn("panel setup begin", "err", err)
		}
		writeErr(w, http.StatusBadRequest, "invalid or expired setup link")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (h *handler) setupFinish(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token    string `json:"token"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, p, err := h.acc.SetupFinish(r.Context(), accounts.SetupFinishRequest{
		Token: in.Token, Password: in.Password, Code: in.Code,
		ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.authFail(w, err, "setup")
		return
	}
	h.startSession(w, id, p)
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, p, err := h.acc.Login(r.Context(), accounts.LoginRequest{
		Username: in.Username, Password: in.Password, Code: in.Code,
		ClientIP: clientIP(r), UserAgent: r.UserAgent(),
	})
	if err != nil {
		h.authFail(w, err, "login")
		return
	}
	h.startSession(w, id, p)
}

func (h *handler) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(server.SessionCookie); err == nil && c.Value != "" {
		if err := h.acc.Logout(r.Context(), c.Value); err != nil {
			h.log.Warn("panel logout", "err", err)
		}
	}
	setCookie(w, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) session(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	c, err := r.Cookie(server.SessionCookie)
	if err != nil || c.Value == "" {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	p, err := h.acc.Session(r.Context(), c.Value)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"username": p.Name, "role": p.Role.String()})
}
