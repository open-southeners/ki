package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type keyKind int

const (
	keyEC keyKind = iota
	keyECPKCS8
	keyRSAPKCS1
	keyRSAPKCS8
	keyEd25519
)

// selfCert makes a self-signed leaf for names and the PEM of its key.
func selfCert(t *testing.T, kind keyKind, names []string, nb, na time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	var signer crypto.Signer
	var err error
	switch kind {
	case keyEC, keyECPKCS8:
		signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case keyRSAPKCS1, keyRSAPKCS8:
		signer, err = rsa.GenerateKey(rand.Reader, 2048)
	case keyEd25519:
		_, signer, err = ed25519.GenerateKey(rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "byo"},
		DNSNames: names, NotBefore: nb, NotAfter: na,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), signer)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	switch kind {
	case keyEC:
		b, _ := x509.MarshalECPrivateKey(signer.(*ecdsa.PrivateKey))
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b})
	case keyRSAPKCS1:
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(signer.(*rsa.PrivateKey))})
	default:
		b, err := x509.MarshalPKCS8PrivateKey(signer)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b})
	}
	return certPEM, keyPEM
}

func validWindow() (time.Time, time.Time) {
	return time.Now().Add(-time.Hour), time.Now().Add(90 * 24 * time.Hour)
}

func TestImportKeyKinds(t *testing.T) {
	for name, kind := range map[string]keyKind{"ec": keyEC, "ec-pkcs8": keyECPKCS8, "rsa-pkcs1": keyRSAPKCS1, "rsa-pkcs8": keyRSAPKCS8, "ed25519": keyEd25519} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			nb, na := validWindow()
			c, k := selfCert(t, kind, []string{"*.example.com", "example.com"}, nb, na)
			imp, err := Import(dir, c, k, false, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(imp.Names, ","); got != "*.example.com,example.com" {
				t.Errorf("names = %s", got)
			}
			if !ValidID(imp.ID) {
				t.Errorf("id %q", imp.ID)
			}
			for _, ext := range []string{".crt", ".key"} {
				st, err := os.Stat(filepath.Join(dir, "certs", "imported", imp.ID+ext))
				if err != nil || st.Mode().Perm() != 0o600 {
					t.Errorf("%s: %v %v", ext, st, err)
				}
			}
		})
	}
}

func TestImportRejects(t *testing.T) {
	now := time.Now()
	nb, na := validWindow()
	good, goodKey := selfCert(t, keyEC, []string{"a.example.com"}, nb, na)
	_, otherKey := selfCert(t, keyEC, []string{"a.example.com"}, nb, na)
	expired, expiredKey := selfCert(t, keyEC, []string{"a.example.com"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	soon, soonKey := selfCert(t, keyEC, []string{"a.example.com"}, now.Add(-time.Hour), now.Add(12*time.Hour))
	future, futureKey := selfCert(t, keyEC, []string{"a.example.com"}, now.Add(time.Hour), na)
	deep, deepKey := selfCert(t, keyEC, []string{"*.*.example.com"}, nb, na)
	mid, midKey := selfCert(t, keyEC, []string{"a.*.example.com"}, nb, na)
	part, partKey := selfCert(t, keyEC, []string{"w*.example.com"}, nb, na)
	bare, bareKey := selfCert(t, keyEC, []string{"*.com"}, nb, na)
	noSAN, noSANKey := selfCert(t, keyEC, nil, nb, na)
	upper, upperKey := selfCert(t, keyEC, []string{"a.example.com", "Bad_Name.example.com"}, nb, na)

	tests := []struct {
		name      string
		cert, key []byte
		want      string
	}{
		{"mismatch", good, otherKey, "does not match"},
		{"expired", expired, expiredKey, "expires too soon"},
		{"near expiry", soon, soonKey, "expires too soon"},
		{"not yet valid", future, futureKey, "not valid yet"},
		{"wildcard two labels", deep, deepKey, "wildcard"},
		{"wildcard not leftmost", mid, midKey, "wildcard"},
		{"partial wildcard", part, partKey, "wildcard"},
		{"wildcard over TLD", bare, bareKey, "wildcard"},
		{"no SAN", noSAN, noSANKey, "no DNS"},
		{"invalid name", upper, upperKey, "invalid character"},
		{"garbage cert", []byte("nope"), goodKey, "no PEM"},
		{"garbage key", good, []byte("nope"), "no PEM"},
		{"bad key block", good, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")}), "unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := Import(dir, tt.cert, tt.key, false, now)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if _, err := os.Stat(filepath.Join(dir, "certs")); err == nil {
				t.Error("rejected import wrote files")
			}
		})
	}
}

func TestImportReplaceListRemove(t *testing.T) {
	dir := t.TempDir()
	nb, na := validWindow()
	c1, k1 := selfCert(t, keyEC, []string{"*.example.com", "example.com"}, nb, na)
	c2, k2 := selfCert(t, keyEC, []string{"example.com", "*.example.com"}, nb, na.Add(24*time.Hour))
	other, otherKey := selfCert(t, keyEC, []string{"*.example.org"}, nb, na)

	a, err := Import(dir, c1, k1, true, time.Now())
	if err != nil || !a.Server {
		t.Fatal(a, err)
	}
	b, err := Import(dir, c2, k2, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("ids differ: %s %s", a.ID, b.ID)
	}
	o, err := Import(dir, other, otherKey, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	list, err := ListImported(dir)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v %v", list, err)
	}
	for _, l := range list {
		if l.ID == a.ID && (l.Server || !l.NotAfter.Equal(b.NotAfter)) {
			t.Errorf("replace did not take effect: %+v", l)
		}
	}
	// Installed List ignores the imported subdirectory.
	if inst, err := List(dir); err != nil || len(inst) != 0 {
		t.Errorf("installed list = %+v %v", inst, err)
	}
	if err := RemoveImported(dir, o.ID); err != nil {
		t.Fatal(err)
	}
	if err := RemoveImported(dir, o.ID); err == nil {
		t.Error("second remove should fail")
	}
	if err := RemoveImported(dir, "../x"); err == nil {
		t.Error("invalid id accepted")
	}
	if list, _ := ListImported(dir); len(list) != 1 {
		t.Errorf("list after remove = %+v", list)
	}
	if empty, err := ListImported(t.TempDir()); err != nil || len(empty) != 0 {
		t.Errorf("empty list = %+v %v", empty, err)
	}
}

func TestCovers(t *testing.T) {
	names := []string{"*.example.com", "plain.example.org"}
	for d, want := range map[string]bool{
		"a.example.com": true, "example.com": false, "a.b.example.com": false,
		"plain.example.org": true, "x.example.org": false, ".example.com": false,
	} {
		if got := Covers(names, d); got != want {
			t.Errorf("Covers(%q) = %v, want %v", d, got, want)
		}
	}
}
