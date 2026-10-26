package edge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/edge/certs"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/store"
)

// byoPair returns a self-signed certificate and key PEM for names.
func byoPair(t *testing.T, names ...string) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "byo"}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}))
}

func TestHandlersImportFlow(t *testing.T) {
	e := newHEnv(t)
	c := e.client(t, rpc.TransportUnix)
	crt, key := byoPair(t, "*.example.com", "ops.example.com")

	var st Status
	if err := call(t, c, MethodImport, map[string]any{"certificate": crt, "key": key, "server": true}, &st); err != nil {
		t.Fatal(err)
	}
	if e.mgr.triggers.Load() != 1 {
		t.Errorf("triggers = %d", e.mgr.triggers.Load())
	}
	list, err := certs.ListImported(e.dir)
	if err != nil || len(list) != 1 || !list[0].Server {
		t.Fatalf("list = %+v %v", list, err)
	}
	id := list[0].ID
	if _, err := os.Stat(filepath.Join(e.dir, "certs", "imported", id+".key")); err != nil {
		t.Fatal(err)
	}

	// Remove by id; an unknown id is refused.
	wantCode(t, call(t, c, MethodRemove, map[string]string{"id": "i-000000000000"}, nil), rpc.CodeInvalidParams)
	if err := call(t, c, MethodRemove, map[string]string{"id": id}, &st); err != nil {
		t.Fatal(err)
	}
	if list, _ := certs.ListImported(e.dir); len(list) != 0 {
		t.Errorf("not removed: %+v", list)
	}
}

func TestHandlersImportErrors(t *testing.T) {
	e := newHEnv(t)
	c := e.client(t, rpc.TransportUnix)
	crt, key := byoPair(t, "app.example.com")
	_, otherKey := byoPair(t, "app.example.com")
	wild, wildKey := byoPair(t, "*.example.com")

	tests := []struct {
		name   string
		params map[string]any
		code   int
		msg    string
	}{
		{"missing key", map[string]any{"certificate": crt}, rpc.CodeInvalidParams, "required"},
		{"mismatch", map[string]any{"certificate": crt, "key": otherKey}, rpc.CodeInvalidParams, "does not match"},
		{"server hostname exposed by a project", map[string]any{"certificate": crt, "key": key, "server": true}, rpc.CodeAlreadyExists, "cannot be a server hostname"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re := wantCode(t, call(t, c, MethodImport, tt.params, nil), tt.code)
			if !strings.Contains(re.Message, tt.msg) {
				t.Errorf("message = %q, want %q", re.Message, tt.msg)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(e.dir, "certs")); !os.IsNotExist(err) {
		t.Error("a refused import wrote files")
	}
	// Without the server flag (and for wildcards) project domains never conflict.
	if err := call(t, c, MethodImport, map[string]any{"certificate": crt, "key": key}, nil); err != nil {
		t.Fatalf("non-server import of a project domain: %v", err)
	}
	if err := call(t, c, MethodImport, map[string]any{"certificate": wild, "key": wildKey, "server": true}, nil); err != nil {
		t.Fatalf("server wildcard import: %v", err)
	}
}

func TestHandlersImportTransport(t *testing.T) {
	e := newHEnv(t)
	crt, key := byoPair(t, "a.example.com")
	for _, tr := range []string{rpc.TransportWebSocket, rpc.TransportControlPlane} {
		c := e.client(t, tr)
		re := wantCode(t, call(t, c, MethodImport, map[string]any{"certificate": crt, "key": key}, nil), rpc.CodeUnauthorized)
		if re.Message != "only available on: unix" {
			t.Errorf("%s message = %q", tr, re.Message)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "certs")); !os.IsNotExist(err) {
		t.Error("a refused call touched the certs directory")
	}
}

func TestStatusAndServerHostnamesWithImported(t *testing.T) {
	cfg := enabledCfg()
	r := newRig(t, cfg)
	crt, key := byoPair(t, "*.example.com", "ops.example.com")
	if _, err := certs.Import(r.edge, []byte(crt), []byte(key), true, time.Now()); err != nil {
		t.Fatal(err)
	}
	crt2, key2 := byoPair(t, "*.example.org")
	other, err := certs.Import(r.edge, []byte(crt2), []byte(key2), false, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	hosts := r.m.ServerHostnames()
	if !slices.Equal(hosts, []string{"ops.example.com", "panel.example.com"}) {
		t.Errorf("server hostnames = %v", hosts)
	}
	s := r.m.Status()
	var imported, acme int
	for _, c := range s.Certificates {
		switch c.Source {
		case SourceImported:
			imported++
			if c.ID == other.ID && (c.Server || c.Names[0] != "*.example.org" || c.NotAfter == nil) {
				t.Errorf("entry = %+v", c)
			}
			if c.ID != other.ID && (!c.Server || !slices.Equal(c.Names, []string{"*.example.com", "ops.example.com"})) {
				t.Errorf("entry = %+v", c)
			}
		case SourceACME:
			acme++
		}
	}
	if imported != 2 {
		t.Errorf("certificates = %+v", s.Certificates)
	}
}

func TestReconcileImportedCoversDomains(t *testing.T) {
	r := newRig(t, enabledCfg())
	crt, key := byoPair(t, "*.example.com")
	imp, err := certs.Import(r.edge, []byte(crt), []byte(key), false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	web, det := webContainer("w1", "172.20.0.2")
	r.rt.set(web)
	r.rt.details["w1"] = det
	r.ex.list = []store.ProjectExposure{{Project: "shop", Exposure: store.Exposure{Service: "web", Port: 8080, Domains: []string{"shop.example.com", "shop.example.org"}}}}
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.m.Close()
	waitFor(t, "running", func() bool { return r.m.Status().State == StateRunning })

	routes, err := os.ReadFile(filepath.Join(r.edge, "dynamic", "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(routes), "/etc/traefik/certs/imported/"+imp.ID+".crt") {
		t.Errorf("imported certificate not loaded:\n%s", routes)
	}
	// shop.example.org is not covered, so the route keeps the resolver.
	if !strings.Contains(string(routes), "certResolver: le") {
		t.Errorf("partially covered route lost its resolver:\n%s", routes)
	}
	var acme []string
	for _, c := range r.m.Status().Certificates {
		if c.Source == SourceACME {
			acme = append(acme, c.Hostname)
		}
	}
	if !slices.Equal(acme, []string{"shop.example.org"}) {
		t.Errorf("acme entries = %v, want only the uncovered domain", acme)
	}
}
