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
	"io"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/rpc/rpctest"
	"github.com/open-southeners/ki-agent/internal/store"
)

type stubMgr struct {
	triggers atomic.Int32
	status   Status
}

func (s *stubMgr) Status() Status { return s.status }
func (s *stubMgr) Trigger()       { s.triggers.Add(1) }

type hEnv struct {
	mgr *stubMgr
	dir string
	h   *Handlers
	r   *rpc.Router
}

func newHEnv(t *testing.T) *hEnv {
	t.Helper()
	e := &hEnv{mgr: &stubMgr{status: Status{Enabled: true, State: "running", Routes: 2}}, dir: t.TempDir(), r: rpc.NewRouter()}
	e.h = NewHandlers(e.mgr, e.dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.h.DomainOwner = func(_ context.Context, d string) (string, string, error) {
		if d == "app.example.com" {
			return "shop", "web", nil
		}
		return "", "", store.ErrNotFound
	}
	e.h.Register(e.r)
	return e
}

// client returns a peer calling the handlers as served over transport.
func (e *hEnv) client(t *testing.T, transport string) *rpc.Peer {
	t.Helper()
	ca, cb := rpctest.Pipe()
	srv := rpc.NewPeer(ca, e.r)
	rpc.SetTransport(srv, transport)
	go srv.Run(context.Background())
	cl := rpc.NewPeer(cb, rpc.NewRouter())
	go cl.Run(context.Background())
	t.Cleanup(func() { cl.Close(); srv.Close() })
	return cl
}

func call(t *testing.T, p *rpc.Peer, method string, params, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return p.Call(ctx, method, params, out)
}

func wantCode(t *testing.T, err error, code int) *rpc.Error {
	t.Helper()
	var re *rpc.Error
	if !errors.As(err, &re) || re.Code != code {
		t.Fatalf("err = %v, want code %d", err, code)
	}
	return re
}

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newCA(t *testing.T) *testCA {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(1000 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &testCA{c, key}
}

func (ca *testCA) sign(t *testing.T, csrPEM string, name string) string {
	t.Helper()
	b, _ := pem.Decode([]byte(csrPEM))
	if b == nil {
		t.Fatal("bad CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatalf("csr invalid: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: csr.Subject, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestHandlersCertificateFlow(t *testing.T) {
	e := newHEnv(t)
	c := e.client(t, rpc.TransportUnix)
	ca := newCA(t)

	var csr struct{ Hostname, CSR string }
	if err := call(t, c, MethodCSR, map[string]string{"hostname": "srv.example.com"}, &csr); err != nil {
		t.Fatal(err)
	}
	if csr.Hostname != "srv.example.com" || !strings.Contains(csr.CSR, "CERTIFICATE REQUEST") {
		t.Fatalf("csr = %+v", csr)
	}
	if e.mgr.triggers.Load() != 0 {
		t.Error("csr must not trigger a reconcile")
	}

	var st Status
	err := call(t, c, MethodInstall, map[string]string{"hostname": "srv.example.com", "certificate": ca.sign(t, csr.CSR, "srv.example.com")}, &st)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "running" || st.Routes != 2 || e.mgr.triggers.Load() != 1 {
		t.Errorf("status = %+v triggers = %d", st, e.mgr.triggers.Load())
	}
	crt := filepath.Join(e.dir, "certs", "srv.example.com.crt")
	if _, err := os.Stat(crt); err != nil {
		t.Fatal(err)
	}

	if err := call(t, c, MethodRemove, map[string]string{"hostname": "srv.example.com"}, &st); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(crt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("certificate not removed: %v", err)
	}
	if e.mgr.triggers.Load() != 2 {
		t.Errorf("triggers = %d", e.mgr.triggers.Load())
	}
}

func TestHandlersErrors(t *testing.T) {
	e := newHEnv(t)
	c := e.client(t, rpc.TransportControlPlane)
	ca := newCA(t)
	var csr struct{ CSR string }
	if err := call(t, c, MethodCSR, map[string]string{"hostname": "srv.example.com"}, &csr); err != nil {
		t.Fatal(err)
	}
	good := ca.sign(t, csr.CSR, "srv.example.com")

	tests := []struct {
		name, method string
		params       map[string]string
		code         int
		msg          string
	}{
		{"csr bad hostname", MethodCSR, map[string]string{"hostname": "Bad_Host"}, rpc.CodeInvalidParams, "invalid params"},
		{"csr empty hostname", MethodCSR, map[string]string{}, rpc.CodeInvalidParams, "invalid params"},
		{"csr exposed domain", MethodCSR, map[string]string{"hostname": "app.example.com"}, rpc.CodeAlreadyExists, `project "shop"`},
		{"install exposed domain", MethodInstall, map[string]string{"hostname": "app.example.com", "certificate": good}, rpc.CodeAlreadyExists, "cannot be a server hostname"},
		{"remove exposed domain", MethodRemove, map[string]string{"hostname": "app.example.com"}, rpc.CodeAlreadyExists, "shop"},
		{"install no certificate", MethodInstall, map[string]string{"hostname": "srv.example.com"}, rpc.CodeInvalidParams, "certificate is required"},
		{"install garbage", MethodInstall, map[string]string{"hostname": "srv.example.com", "certificate": "nope"}, rpc.CodeInvalidParams, "invalid params"},
		{"install no key", MethodInstall, map[string]string{"hostname": "other.example.com", "certificate": good}, rpc.CodeInvalidParams, "request a CSR first"},
		{"install wrong name", MethodInstall, map[string]string{"hostname": "srv.example.com", "certificate": ca.sign(t, csr.CSR, "x.example.com")}, rpc.CodeInvalidParams, "does not cover"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re := wantCode(t, call(t, c, tt.method, tt.params, nil), tt.code)
			if !strings.Contains(re.Message, tt.msg) {
				t.Errorf("message = %q, want %q", re.Message, tt.msg)
			}
		})
	}
	if n := e.mgr.triggers.Load(); n != 0 {
		t.Errorf("failures triggered %d reconciles", n)
	}
}

func TestHandlersTransports(t *testing.T) {
	e := newHEnv(t)
	ws := e.client(t, rpc.TransportWebSocket)

	var st Status
	if err := call(t, ws, MethodStatus, nil, &st); err != nil || st.Routes != 2 {
		t.Fatalf("status over websocket: %+v %v", st, err)
	}
	for _, m := range []string{MethodCSR, MethodInstall, MethodRemove} {
		re := wantCode(t, call(t, ws, m, map[string]string{"hostname": "srv.example.com"}, nil), rpc.CodeUnauthorized)
		if re.Message != "only available on: unix, control-plane" {
			t.Errorf("%s message = %q", m, re.Message)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "certs")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused call touched the certs directory")
	}
}
