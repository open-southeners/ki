package cli

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/edge/certs"
	"github.com/open-southeners/ki/internal/rpc"
)

func TestEdgeCertificateImportRemove(t *testing.T) {
	// A real (self-signed) pair so the CLI can derive the id for its summary.
	dir := t.TempDir()
	crt, key := writeTestPair(t, dir)
	names, err := certs.ImportNames(mustRead(t, crt), mustRead(t, key), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id := certs.ImportedID(names)

	rec := &calls{}
	status := `{"enabled":true,"certificates":[{"id":"` + id + `","hostname":"*.example.com","names":["*.example.com"],"source":"imported","server":true,"not_after":"2027-01-01T00:00:00Z"}]}`
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle(stub(rec, "edge.certificate.import", status))
		r.Handle(stub(rec, "edge.certificate.remove", status))
	})

	out, _, err := runCLI(t, sock, "", "edge", "certificate", "import", "--cert", crt, "--key", key, "--server")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Imported "+id) || !strings.Contains(out, "*.example.com") {
		t.Errorf("output:\n%s", out)
	}
	p := rec.last(t, "edge.certificate.import")
	if p["server"] != true || !strings.Contains(p["certificate"].(string), "BEGIN CERTIFICATE") || p["key"] == "" {
		t.Errorf("params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "edge", "certificate", "import", "--cert", crt); err == nil {
		t.Error("missing --key should fail")
	}
	if _, _, err := runCLI(t, sock, "", "edge", "certificate", "import", "--cert", crt, "--key", filepath.Join(dir, "none")); err == nil {
		t.Error("unreadable key should fail")
	}

	if _, _, err := runCLI(t, sock, "", "edge", "certificate", "remove", id); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "edge.certificate.remove"); p["id"] != id || p["hostname"] != nil {
		t.Errorf("remove by id params = %v", p)
	}
	if _, _, err := runCLI(t, sock, "", "edge", "certificate", "remove", "srv.example.com"); err != nil {
		t.Fatal(err)
	}
	if p := rec.last(t, "edge.certificate.remove"); p["hostname"] != "srv.example.com" || p["id"] != nil {
		t.Errorf("remove by hostname params = %v", p)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeTestPair writes a self-signed certificate for *.example.com and its
// key into dir and returns their paths.
func writeTestPair(t *testing.T, dir string) (crt, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "byo"}, DNSNames: []string{"*.example.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(90 * 24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	crt, key = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return crt, key
}
