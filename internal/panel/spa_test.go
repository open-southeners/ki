package panel

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

const wantSPACSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func spaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            {Data: []byte(`<!doctype html><div id="root"></div><script type="module" src="/assets/index-abc.js"></script>`)},
		"assets/index-abc.js":   {Data: []byte(`console.log("app")`)},
		"assets/index-abc.css":  {Data: []byte(`body{}`)},
		"assets/font-xyz.woff2": {Data: []byte("wOF2")},
		"favicon.svg":           {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"theme-init.js":         {Data: []byte(`/* theme */`)},
		"README.md":             {Data: []byte("placeholder")},
		".keep":                 {Data: nil},
	}
}

func newSPA(s *stub, assets fstest.MapFS) http.Handler {
	return New(Options{Accounts: s, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Assets: assets})
}

func TestSPARoutesServeIndex(t *testing.T) {
	h := newSPA(&stub{}, spaFS())
	for _, p := range []string{"/", "/setup", "/login", "/projects", "/projects/my-app", "/projects/my-app/deploys/3", "/admin/users", "/index.html", "/does/not/exist"} {
		w := do(h, "GET", p, "", nil)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `id="root"`) {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s content type = %q", p, ct)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s cache-control = %q", p, cc)
		}
	}
	if w := do(h, "HEAD", "/projects/x", "", nil); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d", w.Code, w.Body.Len())
	}
}

func TestSPAAssets(t *testing.T) {
	h := newSPA(&stub{}, spaFS())
	tests := []struct {
		path, ctype, cache string
	}{
		{"/assets/index-abc.js", "text/javascript", "public, max-age=31536000, immutable"},
		{"/assets/index-abc.css", "text/css", "public, max-age=31536000, immutable"},
		{"/assets/font-xyz.woff2", "font/woff2", "public, max-age=31536000, immutable"},
		{"/favicon.svg", "image/svg+xml", "no-cache"},
		{"/theme-init.js", "text/javascript", "no-cache"},
	}
	for _, tt := range tests {
		w := do(h, "GET", tt.path, "", nil)
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), tt.ctype) || w.Header().Get("Cache-Control") != tt.cache {
			t.Fatalf("%s: %d %q %q", tt.path, w.Code, w.Header().Get("Content-Type"), w.Header().Get("Cache-Control"))
		}
		if w.Body.Len() == 0 {
			t.Fatalf("%s: empty body", tt.path)
		}
	}
}

func TestSPANotFound(t *testing.T) {
	h := newSPA(&stub{}, spaFS())
	for _, p := range []string{
		"/assets/missing-123.js", // unknown hashed asset
		"/assets/",               // directory listing
		"/assets",                // directory
		"/missing.png",           // looks like a file
		"/auth/nope",             // reserved prefixes never fall back to the app
		"/auth/",
		"/rpc",
		"/hooks/github",
		"/hooks/",
		"/README.md", // dist placeholders are not served
		"/.keep",
	} {
		w := do(h, "GET", p, "", nil)
		if w.Code != 404 || strings.Contains(w.Body.String(), `id="root"`) {
			t.Fatalf("%s: %d %q", p, w.Code, w.Body)
		}
		if !strings.Contains(w.Header().Get("Content-Type"), "json") {
			t.Fatalf("%s: content type %q", p, w.Header().Get("Content-Type"))
		}
	}
	if w := do(h, "POST", "/projects", `{}`, nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST page: %d %q", w.Code, w.Header().Get("Allow"))
	}
}

func TestSPAHeaders(t *testing.T) {
	h := newSPA(&stub{}, spaFS())
	for _, p := range []string{"/", "/projects/x", "/assets/index-abc.js", "/nope", "/auth/session"} {
		w := do(h, "GET", p, "", nil)
		want := map[string]string{
			"Content-Security-Policy": wantSPACSP,
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
	if cspSPA != wantSPACSP {
		t.Fatalf("cspSPA drifted: %q", cspSPA)
	}
}

func TestSPAKeepsAuthEndpoints(t *testing.T) {
	h := newSPA(&stub{}, spaFS())
	w := do(h, "POST", "/auth/login", `{"username":"ann","password":"pw","code":"123456"}`, nil)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	w = do(h, "GET", "/auth/session", "", map[string]string{"Cookie": "__Host-ki_session=sid1"})
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("session: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
}

func TestFallsBackToStaticPage(t *testing.T) {
	// No index.html (only the committed placeholders): the minimal page.
	h := newSPA(&stub{}, fstest.MapFS{"README.md": {Data: []byte("x")}})
	w := do(h, "GET", "/", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "<form") {
		t.Fatalf("static page: %d", w.Code)
	}
	if w.Header().Get("Content-Security-Policy") != cspStatic {
		t.Fatalf("csp = %q", w.Header().Get("Content-Security-Policy"))
	}
	if w := do(h, "GET", "/projects/x", "", nil); w.Code != 404 {
		t.Fatalf("static mode has no SPA fallback: %d", w.Code)
	}
}

func TestEmbeddedDist(t *testing.T) {
	// Without injected assets the embedded dist is used: the real app when it
	// was built, else the minimal page. Either way "/" is served.
	h := New(Options{Accounts: &stub{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	w := do(h, "GET", "/", "", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("/: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	csp := w.Header().Get("Content-Security-Policy")
	if csp != cspStatic && csp != cspSPA {
		t.Fatalf("csp = %q", csp)
	}
	if strings.Contains(w.Body.String(), `id="root"`) != (csp == cspSPA) {
		t.Fatal("page and CSP disagree about the mode")
	}
}
