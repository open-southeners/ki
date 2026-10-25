package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidHostname(t *testing.T) {
	long := strings.Repeat("a", 64) + ".com"
	tests := []struct {
		name string
		ok   bool
	}{
		{"a.example.com", true},
		{"xn--bcher-kva.example", true},
		{"a-b.c1.io", true},
		{"", false},
		{"localhost", false},
		{"Example.com", false},
		{"*.example.com", false},
		{"1.2.3.4", false},
		{"::1", false},
		{"bücher.example", false},
		{"-a.example.com", false},
		{"a-.example.com", false},
		{"a..com", false},
		{"a.com.", false},
		{"a_b.com", false},
		{"a/b.com", false},
		{"../x.com", false},
		{long, false},
		{strings.Repeat("a.", 130) + "com", false},
	}
	for _, tt := range tests {
		if err := ValidHostname(tt.name); (err == nil) != tt.ok {
			t.Errorf("ValidHostname(%q) = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
}

func TestSelfSigned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "edge")
	lister := func() ([]net.Addr, error) {
		return []net.Addr{
			&net.IPNet{IP: net.ParseIP("127.0.0.1")},
			&net.IPNet{IP: net.ParseIP("10.1.2.3")},
			&net.IPAddr{IP: net.ParseIP("fd00::5")},
			&net.IPNet{IP: net.ParseIP("10.1.2.3")},
		}, nil
	}
	c, err := loadOrCreateSelfSigned(dir, lister, func() (string, error) { return "box.local", nil })
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(c.KeyFile); st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	leaf, err := loadPair(c.CertFile, c.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA || leaf.KeyUsage != x509.KeyUsageDigitalSignature ||
		len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("bad usage: %+v", leaf)
	}
	if leaf.Subject.CommonName != "ki-agent" || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "box.local" {
		t.Errorf("bad names: %v %v", leaf.Subject, leaf.DNSNames)
	}
	if len(leaf.IPAddresses) != 2 || !leaf.IPAddresses[0].Equal(net.ParseIP("10.1.2.3")) {
		t.Errorf("IPs: %v", leaf.IPAddresses)
	}
	if d := leaf.NotAfter.Sub(leaf.NotBefore); d < 9*365*24*time.Hour {
		t.Errorf("validity %v", d)
	}
	if len(c.FingerprintSHA256) != 32*3-1 || c.FingerprintSHA256 != strings.ToUpper(c.FingerprintSHA256) {
		t.Errorf("fingerprint %q", c.FingerprintSHA256)
	}
	// Second load returns the same cert, even with different SAN sources.
	c2, err := loadOrCreateSelfSigned(dir, func() ([]net.Addr, error) { return nil, nil }, func() (string, error) { return "other", nil })
	if err != nil {
		t.Fatal(err)
	}
	if c2.FingerprintSHA256 != c.FingerprintSHA256 || !c2.NotAfter.Equal(c.NotAfter) {
		t.Errorf("not stable: %+v vs %+v", c, c2)
	}
	if _, err := LoadOrCreateSelfSigned(t.TempDir()); err != nil {
		t.Errorf("real lister: %v", err)
	}
}

func TestSelfSignedErrors(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(t *testing.T, c Cert)
	}{
		{"only key", func(t *testing.T, c Cert) { os.Remove(c.CertFile) }},
		{"only cert", func(t *testing.T, c Cert) { os.Remove(c.KeyFile) }},
		{"garbage cert", func(t *testing.T, c Cert) { os.WriteFile(c.CertFile, []byte("x"), 0o644) }},
		{"garbage key", func(t *testing.T, c Cert) { os.WriteFile(c.KeyFile, []byte("x"), 0o600) }},
		{"mismatch", func(t *testing.T, c Cert) {
			o, _ := LoadOrCreateSelfSigned(t.TempDir())
			b, _ := os.ReadFile(o.KeyFile)
			os.WriteFile(c.KeyFile, b, 0o600)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			c, err := LoadOrCreateSelfSigned(dir)
			if err != nil {
				t.Fatal(err)
			}
			tt.corrupt(t, c)
			before, _ := os.ReadFile(c.KeyFile)
			if _, err := LoadOrCreateSelfSigned(dir); err == nil {
				t.Fatal("want error")
			}
			after, _ := os.ReadFile(c.KeyFile)
			if string(before) != string(after) {
				t.Error("key was overwritten")
			}
		})
	}
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

// sign signs the CSR with the given names and validity, returning leaf+CA PEM.
func (ca *testCA) sign(t *testing.T, csrPEM []byte, names []string, nb, na time.Time) []byte {
	t.Helper()
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		t.Fatalf("bad CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		t.Fatalf("csr invalid: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: csr.Subject,
		DNSNames: names, NotBefore: nb, NotAfter: na,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})...)
}

func TestCSRKeyReuse(t *testing.T) {
	dir := t.TempDir()
	csr1, err := CSR(dir, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "certs", "a.example.com.key")
	st, err := os.Stat(keyFile)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key: %v %v", err, st)
	}
	if st, _ := os.Stat(filepath.Join(dir, "certs")); st.Mode().Perm() != 0o700 {
		t.Errorf("certs dir mode %v", st.Mode().Perm())
	}
	k1, _ := os.ReadFile(keyFile)
	csr2, err := CSR(dir, "a.example.com")
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := os.ReadFile(keyFile)
	if string(k1) != string(k2) {
		t.Error("key not reused")
	}
	parse := func(p []byte) *x509.CertificateRequest {
		b, _ := pem.Decode(p)
		c, err := x509.ParseCertificateRequest(b.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	a, b := parse(csr1), parse(csr2)
	if len(a.DNSNames) != 1 || a.DNSNames[0] != "a.example.com" || !a.PublicKey.(*ecdsa.PublicKey).Equal(b.PublicKey) {
		t.Errorf("csr: %v", a.DNSNames)
	}
	// A corrupt key must not be replaced.
	os.WriteFile(keyFile, []byte("junk"), 0o600)
	if _, err := CSR(dir, "a.example.com"); err == nil {
		t.Error("want error on corrupt key")
	}
	if b, _ := os.ReadFile(keyFile); string(b) != "junk" {
		t.Error("corrupt key overwritten")
	}
	if _, err := CSR(dir, "../x"); err == nil {
		t.Error("want invalid hostname error")
	}
}

func TestInstall(t *testing.T) {
	ca := newCA(t)
	now := time.Now()
	host := "a.example.com"
	good := func(csr []byte) []byte {
		return ca.sign(t, csr, []string{host}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	}
	tests := []struct {
		name    string
		hostArg string
		prepare func(t *testing.T, dir string) []byte
		wantErr string
	}{
		{"ok", host, func(t *testing.T, dir string) []byte { c, _ := CSR(dir, host); return good(c) }, ""},
		{"wildcard cert covers", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return ca.sign(t, c, []string{"*.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
		}, ""},
		{"invalid hostname", "A.com", func(t *testing.T, dir string) []byte { return nil }, "lowercase"},
		{"malformed pem", host, func(t *testing.T, dir string) []byte { CSR(dir, host); return []byte("nope") }, "no PEM"},
		{"non-cert block", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return c
		}, "unexpected PEM block"},
		{"bad der", host, func(t *testing.T, dir string) []byte {
			CSR(dir, host)
			return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("zz")})
		}, "invalid certificate"},
		{"trailing data", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return append(good(c), []byte("garbage")...)
		}, "trailing"},
		{"no pending key", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(t.TempDir(), host)
			return good(c)
		}, "no pending key"},
		{"key mismatch", host, func(t *testing.T, dir string) []byte {
			CSR(dir, host)
			c, _ := CSR(t.TempDir(), host)
			return good(c)
		}, "does not match"},
		{"wrong hostname", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return ca.sign(t, c, []string{"b.example.com"}, now.Add(-time.Hour), now.Add(90*24*time.Hour))
		}, "does not cover"},
		{"expired", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return ca.sign(t, c, []string{host}, now.Add(-48*time.Hour), now.Add(-time.Hour))
		}, "expires too soon"},
		{"near expiry", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return ca.sign(t, c, []string{host}, now.Add(-time.Hour), now.Add(23*time.Hour))
		}, "expires too soon"},
		{"not yet valid", host, func(t *testing.T, dir string) []byte {
			c, _ := CSR(dir, host)
			return ca.sign(t, c, []string{host}, now.Add(time.Hour), now.Add(90*24*time.Hour))
		}, "not valid yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			chain := tt.prepare(t, dir)
			inst, err := Install(dir, tt.hostArg, chain, now)
			crt := filepath.Join(dir, "certs", tt.hostArg+".crt")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if _, serr := os.Stat(crt); serr == nil {
					t.Error("cert written despite error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if inst.Hostname != tt.hostArg || inst.NotAfter.Before(now.Add(24*time.Hour)) {
				t.Errorf("installed %+v", inst)
			}
			data, _ := os.ReadFile(crt)
			chainOut, err := parseChain(data)
			if err != nil || len(chainOut) != 2 {
				t.Errorf("stored chain: %v %d", err, len(chainOut))
			}
			list, err := List(dir)
			if err != nil || len(list) != 1 || list[0].Hostname != tt.hostArg || !list[0].NotAfter.Equal(inst.NotAfter) {
				t.Errorf("list: %v %v", list, err)
			}
		})
	}
}

func TestListRemove(t *testing.T) {
	dir := t.TempDir()
	if l, err := List(dir); err != nil || len(l) != 0 {
		t.Fatalf("empty list: %v %v", l, err)
	}
	ca := newCA(t)
	now := time.Now()
	for _, h := range []string{"b.example.com", "a.example.com"} {
		c, _ := CSR(dir, h)
		if _, err := Install(dir, h, ca.sign(t, c, []string{h}, now.Add(-time.Hour), now.Add(72*time.Hour)), now); err != nil {
			t.Fatal(err)
		}
	}
	CSR(dir, "pending.example.com") // key only: not listed
	os.WriteFile(filepath.Join(dir, "certs", "junk.example.com.crt"), []byte("x"), 0o644)
	l, err := List(dir)
	if err != nil || len(l) != 2 || l[0].Hostname != "a.example.com" || l[1].Hostname != "b.example.com" {
		t.Fatalf("list: %v %v", l, err)
	}
	if err := Remove(dir, "a.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, "a.example.com"); err != nil {
		t.Errorf("second remove: %v", err)
	}
	if err := Remove(dir, "../etc"); err == nil {
		t.Error("want invalid hostname error")
	}
	for _, ext := range []string{".crt", ".key"} {
		if _, err := os.Stat(filepath.Join(dir, "certs", "a.example.com"+ext)); err == nil {
			t.Errorf("%s still present", ext)
		}
	}
	if l, _ := List(dir); len(l) != 1 {
		t.Errorf("list after remove: %v", l)
	}
}

func TestValidHostnameNumericLastLabel(t *testing.T) {
	if err := ValidHostname("example.123"); err == nil {
		t.Error("numeric last label accepted")
	}
}
