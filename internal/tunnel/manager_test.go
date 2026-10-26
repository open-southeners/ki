package tunnel

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/runtime"
)

const testToken = "eyJhIjoiU0VDUkVUU0VDUkVUIiwidCI6InRva2VuLXRva2VuIn0="

type fakeCompose struct {
	mu    sync.Mutex
	ups   int
	files []string
	err   error
	onUp  func()
}

func (f *fakeCompose) Up(_ context.Context, file, project string, out func(string, string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if project != ProjectName {
		return errors.New("wrong project " + project)
	}
	f.ups++
	f.files = append(f.files, file)
	if f.err != nil {
		out("stderr", "Error: pull access denied")
		return f.err
	}
	if f.onUp != nil {
		f.onUp()
	}
	return nil
}

func (f *fakeCompose) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ups
}

type fakeRuntime struct {
	mu         sync.Mutex
	containers []runtime.Container
	removed    []string
}

func (f *fakeRuntime) List(context.Context, string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.Container(nil), f.containers...), nil
}

func (f *fakeRuntime) Remove(_ context.Context, id string, force bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !force {
		return errors.New("want force")
	}
	f.removed = append(f.removed, id)
	f.containers = nil
	return nil
}

func (f *fakeRuntime) set(c ...runtime.Container) {
	f.mu.Lock()
	f.containers = c
	f.mu.Unlock()
}

func tunnelContainer(state string) runtime.Container {
	return runtime.Container{ID: "tun1", State: state, Labels: map[string]string{
		runtime.LabelManaged: "true", LabelComponent: ComponentTunnel,
	}}
}

type rig struct {
	m    *Manager
	c    *fakeCompose
	rt   *fakeRuntime
	dir  string
	logs *bytes.Buffer
	srv  *httptest.Server
	// ready is the status code and body the fake /ready endpoint returns.
	mu    sync.Mutex
	code  int
	body  string
	probe int
}

func newRig(t *testing.T, cfg config.Tunnel, edgeMode string) *rig {
	t.Helper()
	r := &rig{c: &fakeCompose{}, rt: &fakeRuntime{}, dir: t.TempDir(), logs: &bytes.Buffer{}, code: 503, body: `{"status":503,"readyConnections":0}`}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.probe++
		w.WriteHeader(r.code)
		_, _ = w.Write([]byte(r.body))
	}))
	t.Cleanup(r.srv.Close)
	r.c.onUp = func() { r.rt.set(tunnelContainer("running")) }
	r.m = New(Options{
		Config: cfg, DataDir: r.dir, UID: os.Getuid(), GID: os.Getgid(),
		EdgeEnabled: true, EdgeMode: edgeMode,
		Compose: r.c, Runtime: r.rt,
		Logger:   slog.New(slog.NewTextHandler(r.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		interval: 20 * time.Millisecond, minBackoff: 20 * time.Millisecond, maxBackoff: 40 * time.Millisecond,
		readyURL: r.srv.URL + "/ready",
	})
	return r
}

func (r *rig) setReady(code int, body string) {
	r.mu.Lock()
	r.code, r.body = code, body
	r.mu.Unlock()
}

func (r *rig) start(t *testing.T) {
	t.Helper()
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.m.Close() })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// noToken fails the test when s mentions the token anywhere.
func noToken(t *testing.T, where, s string) {
	t.Helper()
	if strings.Contains(s, testToken) || strings.Contains(s, "SECRETSECRET") {
		t.Errorf("%s contains the token:\n%s", where, s)
	}
}

func TestDisabledAndUnconfigured(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: false, MetricsPort: 20241}, config.EdgeModeBehind)
	r.start(t)
	st := r.m.Status()
	if st.Enabled || st.State != StateDisabled || len(st.Warnings) != 0 {
		t.Fatalf("disabled status = %+v", st)
	}
	if err := r.m.Configure(testToken); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if r.c.count() != 0 {
		t.Error("a disabled tunnel must not start")
	}

	r = newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeBehind)
	r.start(t)
	time.Sleep(60 * time.Millisecond)
	st = r.m.Status()
	if st.State != StateUnconfigured || st.Configured || r.c.count() != 0 {
		t.Fatalf("unconfigured status = %+v ups=%d", st, r.c.count())
	}
	if _, err := os.Stat(filepath.Join(r.dir, "tunnel", "compose.json")); err == nil {
		t.Error("compose file written without a token")
	}
}

func TestConfigureStartsAndReportsReady(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeBehind)
	r.start(t)
	if err := r.m.Configure(" " + testToken + "\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "compose up", func() bool { return r.c.count() == 1 })
	if st := r.m.Status(); !st.Configured || st.Ready || st.State != StateStarting {
		t.Fatalf("status after start = %+v", st)
	}

	r.setReady(200, `{"status":200,"readyConnections":4,"connectorId":"x"}`)
	st := r.m.Status()
	if st.State != StateConnected || !st.Ready || st.Connections != 4 {
		t.Fatalf("connected status = %+v", st)
	}
	r.setReady(503, `{"status":503,"readyConnections":0}`)
	r.m.mu.Lock()
	r.m.upAt = time.Now().Add(-2 * startGrace)
	r.m.mu.Unlock()
	if st := r.m.Status(); st.State != StateDisconnected || st.Ready || st.Connections != 0 {
		t.Fatalf("disconnected status = %+v", st)
	}

	// Steady state: the container runs and nothing changed, so no restarts.
	time.Sleep(100 * time.Millisecond)
	if n := r.c.count(); n != 1 {
		t.Errorf("compose up ran %d times, want 1", n)
	}

	// A new token recreates the container.
	if err := r.m.Configure(testToken + "AA"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second up", func() bool { return r.c.count() == 2 })

	// A vanished container is started again.
	r.rt.set()
	waitFor(t, "third up", func() bool { return r.c.count() == 3 })
}

func TestTokenNeverLeaks(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeFront)
	r.c.err = errors.New("exit status 1")
	r.start(t)
	if err := r.m.Configure(testToken); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failing up", func() bool { return r.c.count() >= 2 })
	st := r.m.Status()
	if st.State != StateError || st.LastError == "" {
		t.Fatalf("status = %+v", st)
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "behind") {
		t.Errorf("warnings = %v", st.Warnings)
	}
	r.c.mu.Lock()
	r.c.err = nil
	r.c.mu.Unlock()
	waitFor(t, "recovery", func() bool { return r.m.Status().State == StateStarting })

	// Invalid tokens are refused without echoing.
	err := r.m.Configure("bad token;rm -rf")
	if err == nil || strings.Contains(err.Error(), "rm -rf") {
		t.Fatalf("Configure(bad) = %v", err)
	}

	tunnelDir := filepath.Join(r.dir, "tunnel")
	filepath.Walk(tunnelDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		if filepath.Base(p) == "token" {
			if string(b) != testToken {
				t.Errorf("token file = %q", b)
			}
			return nil
		}
		noToken(t, p, string(b))
		return nil
	})
	b, err := os.ReadFile(filepath.Join(tunnelDir, "compose.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--token-file", "/etc/cloudflared/token", "host", "ki.component"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("compose.json lacks %q", want)
		}
	}
	noToken(t, "agent logs", r.logs.String())
	js, _ := jsonOf(r.m.Status())
	noToken(t, "status", js)
	noToken(t, "last error", st.LastError)
}

func TestTokenFilePermissions(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeBehind)
	if err := r.m.Configure(testToken); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.dir, "tunnel", "secret")
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("secret dir: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "token")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v %v", fi, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("secret dir has leftovers: %v", entries)
	}
}

func TestRemove(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeBehind)
	r.start(t)
	if err := r.m.Configure(testToken); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "up", func() bool { return r.c.count() == 1 })
	if err := r.m.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.rt.removed) != 1 || r.rt.removed[0] != "tun1" {
		t.Errorf("removed = %v", r.rt.removed)
	}
	if st := r.m.Status(); st.Configured || st.State != StateUnconfigured {
		t.Errorf("status after remove = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "tunnel", "secret", "token")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("token file remains: %v", err)
	}
	// Idempotent; and the loop does not bring it back without a token.
	if err := r.m.Remove(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if r.c.count() != 1 {
		t.Errorf("tunnel restarted after remove: %d ups", r.c.count())
	}
}

func TestWarnings(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeBehind)
	if w := r.m.Status().Warnings; len(w) != 0 {
		t.Errorf("behind mode warnings = %v", w)
	}
	r.m.opt.EdgeEnabled = false
	if w := r.m.Status().Warnings; len(w) != 1 || !strings.Contains(w[0], "edge is disabled") {
		t.Errorf("edge disabled warnings = %v", w)
	}
}

func TestStartTwice(t *testing.T) {
	r := newRig(t, config.Tunnel{Enabled: true, MetricsPort: 20241}, config.EdgeModeBehind)
	r.start(t)
	if err := r.m.Start(context.Background()); err == nil {
		t.Error("second Start should fail")
	}
}

func TestDefaultReadyURL(t *testing.T) {
	m := New(Options{Config: config.Tunnel{Enabled: true, MetricsPort: 20999}})
	if m.readyURL != "http://127.0.0.1:20999/ready" {
		t.Errorf("readyURL = %q", m.readyURL)
	}
}
