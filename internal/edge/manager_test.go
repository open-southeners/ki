package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/edge/certs"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/store"
)

type fakeCompose struct {
	mu    sync.Mutex
	ups   int
	files []string
	err   error
	// onUp runs after a successful Up (to simulate the container appearing).
	onUp func()
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
		out("stderr", "pulling")
		out("stderr", "Error: port is already allocated")
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
	details    map[string]runtime.ContainerDetail
}

func (f *fakeRuntime) List(_ context.Context, project string) ([]runtime.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Container
	for _, c := range f.containers {
		if project == "" || c.Project == project {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakeRuntime) Inspect(_ context.Context, id string) (runtime.ContainerDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.details[id]
	if !ok {
		return d, runtime.ErrNotFound
	}
	return d, nil
}

func (f *fakeRuntime) set(c ...runtime.Container) {
	f.mu.Lock()
	f.containers = c
	f.mu.Unlock()
}

type fakeExposures struct {
	mu   sync.Mutex
	list []store.ProjectExposure
}

func (f *fakeExposures) AllExposures(context.Context) ([]store.ProjectExposure, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.ProjectExposure(nil), f.list...), nil
}

func edgeContainer(state string) runtime.Container {
	return runtime.Container{ID: "edge1", State: state, Labels: map[string]string{
		runtime.LabelManaged: "true", LabelComponent: ComponentEdge,
	}}
}

func webContainer(id, ip string) (runtime.Container, runtime.ContainerDetail) {
	c := runtime.Container{ID: id, State: "running", Project: "shop", Service: "web", Labels: map[string]string{
		runtime.LabelManaged: "true", runtime.LabelProject: "shop",
		"com.docker.compose.service": "web",
	}}
	return c, runtime.ContainerDetail{Container: c, IPs: map[string]string{"shop_default": ip}}
}

type rig struct {
	m    *Manager
	c    *fakeCompose
	rt   *fakeRuntime
	ex   *fakeExposures
	dir  string
	edge string
}

func newRig(t *testing.T, cfg config.Edge) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{c: &fakeCompose{}, rt: &fakeRuntime{details: map[string]runtime.ContainerDetail{}}, ex: &fakeExposures{}, dir: dir, edge: filepath.Join(dir, "edge")}
	r.c.onUp = func() { r.rt.set(append(r.rt.containers, edgeContainer("running"))...) }
	r.m = New(Options{
		Config: cfg, DataDir: dir, Compose: r.c, Runtime: r.rt, Exposures: r.ex,
		interval: time.Hour, minBackoff: 10 * time.Millisecond, maxBackoff: 40 * time.Millisecond,
	})
	return r
}

func enabledCfg() config.Edge {
	return config.Edge{Enabled: true, HTTPPort: 18080, HTTPSPort: 18443, Domain: "panel.example.com", WebhooksPort: 8090}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestDisabled(t *testing.T) {
	r := newRig(t, config.Edge{})
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.m.Trigger()
	s := r.m.Status()
	if s.Enabled || s.State != StateDisabled || s.ServerHostnames == nil || s.Certificates == nil {
		t.Errorf("status = %+v", s)
	}
	if err := r.m.Close(); err != nil {
		t.Fatal(err)
	}
	if r.c.count() != 0 {
		t.Error("Up called while disabled")
	}
	if _, err := os.Stat(r.edge); !os.IsNotExist(err) {
		t.Error("disabled edge created files")
	}
}

func TestReconcileWritesAndStartsOnce(t *testing.T) {
	r := newRig(t, enabledCfg())
	web, det := webContainer("w1", "172.20.0.2")
	r.rt.set(web)
	r.rt.details["w1"] = det
	r.ex.list = []store.ProjectExposure{{Project: "shop", Exposure: store.Exposure{Service: "web", Port: 8080, Domains: []string{"shop.example.com"}}}}

	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.m.Close()
	if err := r.m.Start(context.Background()); err == nil {
		t.Error("second Start accepted")
	}
	waitFor(t, "running", func() bool { return r.m.Status().State == StateRunning })

	s := r.m.Status()
	if s.Routes != 1 || s.Image != DefaultImage || s.HTTPPort != 18080 || s.SelfSigned.Fingerprint == "" || s.SelfSigned.NotAfter.IsZero() {
		t.Errorf("status = %+v", s)
	}
	if got := strings.Join(s.ServerHostnames, ","); got != "panel.example.com" {
		t.Errorf("server hostnames = %s", got)
	}
	if len(s.Certificates) != 2 || s.Certificates[0].Hostname != "panel.example.com" || s.Certificates[0].Source != SourceACME || s.Certificates[1].Hostname != "shop.example.com" {
		t.Errorf("certificates = %+v", s.Certificates)
	}

	routes, err := os.ReadFile(filepath.Join(r.edge, "dynamic", "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(routes), "http://172.20.0.2:8080") || !strings.Contains(string(routes), "Host(`shop.example.com`)") {
		t.Errorf("routes:\n%s", routes)
	}
	for _, f := range []string{"traefik.yaml", "compose.json", "selfsigned.crt", "selfsigned.key"} {
		if _, err := os.Stat(filepath.Join(r.edge, f)); err != nil {
			t.Error(err)
		}
	}
	fi, err := os.Stat(filepath.Join(r.edge, "acme", "acme.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("acme.json: %v %v", fi, err)
	}
	if fi, _ := os.Stat(filepath.Join(r.edge, "acme")); fi.Mode().Perm() != 0o700 {
		t.Errorf("acme dir mode = %v", fi.Mode())
	}
	ents, _ := os.ReadDir(filepath.Join(r.edge, "dynamic"))
	if len(ents) != 1 {
		t.Errorf("dynamic dir has stray files: %v", ents)
	}

	// Unchanged state: another reconcile writes nothing and does not Up.
	before, _ := os.Stat(filepath.Join(r.edge, "dynamic", "routes.yaml"))
	r.m.Trigger()
	time.Sleep(50 * time.Millisecond)
	after, _ := os.Stat(filepath.Join(r.edge, "dynamic", "routes.yaml"))
	if !before.ModTime().Equal(after.ModTime()) || r.c.count() != 1 {
		t.Errorf("idle reconcile changed things: ups=%d", r.c.count())
	}

	// A new backend shows up in the routes after a Trigger.
	web2, det2 := webContainer("w2", "172.20.0.3")
	r.rt.set(web, web2, edgeContainer("running"))
	r.rt.details["w2"] = det2
	r.m.Trigger()
	waitFor(t, "second backend", func() bool {
		b, _ := os.ReadFile(filepath.Join(r.edge, "dynamic", "routes.yaml"))
		return strings.Contains(string(b), "172.20.0.3:8080")
	})
	if r.c.count() != 1 {
		t.Errorf("route change restarted the edge: ups=%d", r.c.count())
	}

	// The edge container disappearing triggers an Up.
	r.rt.set(web, web2, edgeContainer("exited"))
	r.m.Trigger()
	waitFor(t, "restart", func() bool { return r.c.count() == 2 })
}

func TestStaticChangeRestarts(t *testing.T) {
	r := newRig(t, enabledCfg())
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "running", func() bool { return r.m.Status().State == StateRunning })
	r.m.Close()
	if r.c.count() != 1 {
		t.Fatalf("ups = %d", r.c.count())
	}
	// A new manager (agent restart) with a changed port brings up the new
	// compose project even though the container exists.
	cfg := enabledCfg()
	cfg.HTTPPort = 18081
	m2 := New(Options{Config: cfg, DataDir: r.dir, Compose: r.c, Runtime: r.rt, Exposures: r.ex, interval: time.Hour})
	if err := m2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	waitFor(t, "second up", func() bool { return r.c.count() == 2 })
	b, _ := os.ReadFile(r.c.files[1])
	if !strings.Contains(string(b), `"ki.edge.config"`) {
		t.Errorf("compose lacks config hash label:\n%s", b)
	}
}

func TestErrorBackoffAndRecovery(t *testing.T) {
	r := newRig(t, enabledCfg())
	r.c.err = errors.New("exit status 1")
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.m.Close()
	waitFor(t, "error state", func() bool { return r.m.Status().State == StateError })
	s := r.m.Status()
	if !strings.Contains(s.LastError, "port is already allocated") || strings.Contains(s.LastError, "\n") {
		t.Errorf("last_error = %q", s.LastError)
	}
	waitFor(t, "retries", func() bool { return r.c.count() >= 3 })

	r.c.mu.Lock()
	r.c.err = nil
	r.c.mu.Unlock()
	waitFor(t, "recovery", func() bool { return r.m.Status().State == StateRunning })
	if s := r.m.Status(); s.LastError != "" {
		t.Errorf("last_error not cleared: %q", s.LastError)
	}
}

func TestServerHostnamesAndInstalledRoutes(t *testing.T) {
	r := newRig(t, enabledCfg())
	if got := r.m.ServerHostnames(); len(got) != 1 || got[0] != "panel.example.com" {
		t.Errorf("hostnames = %v", got)
	}
	cfg := enabledCfg()
	cfg.Domain = ""
	m := New(Options{Config: cfg, DataDir: t.TempDir()})
	if got := m.ServerHostnames(); got == nil || len(got) != 0 {
		t.Errorf("hostnames = %#v, want empty non-nil", got)
	}
}

func TestInvalidExposureSkipped(t *testing.T) {
	r := newRig(t, enabledCfg())
	web, det := webContainer("w1", "172.20.0.2")
	r.rt.set(web)
	r.rt.details["w1"] = det
	r.ex.list = []store.ProjectExposure{
		{Project: "shop", Exposure: store.Exposure{Service: "web", Port: 80, Domains: []string{"bad`.example.com"}}},
		{Project: "shop", Exposure: store.Exposure{Service: "web", Port: 80, Domains: []string{"ok.example.com"}}},
	}
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.m.Close()
	waitFor(t, "running", func() bool { return r.m.Status().State == StateRunning })
	if r.m.Status().Routes != 1 {
		t.Errorf("routes = %d", r.m.Status().Routes)
	}
}

func TestWriteIfChanged(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.yaml")
	if ch, err := writeIfChanged(p, []byte("a"), 0o644); err != nil || !ch {
		t.Fatalf("first write: %v %v", ch, err)
	}
	if ch, err := writeIfChanged(p, []byte("a"), 0o644); err != nil || ch {
		t.Fatalf("same content rewritten: %v %v", ch, err)
	}
	if ch, err := writeIfChanged(p, []byte("b"), 0o644); err != nil || !ch {
		t.Fatalf("changed content: %v %v", ch, err)
	}
	ents, _ := os.ReadDir(filepath.Dir(p))
	if len(ents) != 1 {
		t.Errorf("temp files left: %v", ents)
	}
}

func TestStatusReflectsInstallImmediately(t *testing.T) {
	r := newRig(t, enabledCfg())
	if err := r.m.prepare(); err != nil {
		t.Fatal(err)
	}
	const host = "cp.example.net"
	csr, err := certs.CSR(r.edge, host)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(csr)
	req, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	leafTpl := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, req.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	chain := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if _, err := certs.Install(r.edge, host, chain, time.Now()); err != nil {
		t.Fatal(err)
	}
	s := r.m.Status()
	var found bool
	for _, c := range s.Certificates {
		found = found || (c.Hostname == host && c.Source == SourceInstalled && c.NotAfter != nil)
	}
	if !found || !strings.Contains(strings.Join(s.ServerHostnames, ","), host) {
		t.Errorf("after install: %+v", s)
	}
	if err := certs.Remove(r.edge, host); err != nil {
		t.Fatal(err)
	}
	s = r.m.Status()
	for _, c := range s.Certificates {
		if c.Hostname == host {
			t.Errorf("after remove: %+v", s)
		}
	}
}
