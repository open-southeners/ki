package certs

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Imported describes a bring-your-own certificate.
type Imported struct {
	// ID is derived from Names and names the files.
	ID string
	// Names are the leaf's DNS SANs, sorted; wildcards are "*.example.com".
	Names    []string
	NotAfter time.Time
	// Server marks the non-wildcard Names as server hostnames.
	Server bool
}

const (
	importedDir = "imported"
	idPrefix    = "i-"
	idHexLen    = 12
	serverExt   = ".server"
)

// ValidID reports whether s has the shape of an imported certificate id.
func ValidID(s string) bool {
	hexPart, ok := strings.CutPrefix(s, idPrefix)
	if !ok || len(hexPart) != idHexLen {
		return false
	}
	for i := 0; i < len(hexPart); i++ {
		c := hexPart[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ImportedID derives the stable id of a set of names.
func ImportedID(names []string) string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return idPrefix + hex.EncodeToString(sum[:])[:idHexLen]
}

// ValidName checks a certificate name: a valid hostname, or a wildcard
// "*.<hostname>" with the wildcard as the whole left-most label.
func ValidName(name string) error {
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		if strings.Contains(rest, "*") {
			return fmt.Errorf("wildcard %q: only a single left-most label may be a wildcard", name)
		}
		if err := ValidHostname(rest); err != nil {
			return fmt.Errorf("wildcard %q: %w", name, err)
		}
		return nil
	}
	if strings.Contains(name, "*") {
		return fmt.Errorf("wildcard %q: only a single left-most label may be a wildcard", name)
	}
	return ValidHostname(name)
}

// Covers reports whether any of names matches domain: exactly, or through a
// wildcard that stands for exactly one label.
func Covers(names []string, domain string) bool {
	for _, n := range names {
		if n == domain {
			return true
		}
		if rest, ok := strings.CutPrefix(n, "*."); ok {
			label, tail, found := strings.Cut(domain, ".")
			if found && label != "" && tail == rest {
				return true
			}
		}
	}
	return false
}

// checkImport validates the chain, key and validity window and returns the
// parsed chain, the key and the sorted unique DNS names of the leaf.
func checkImport(certPEM, keyPEM []byte, now time.Time) ([]*x509.Certificate, crypto.PrivateKey, []string, error) {
	chain, err := parseChain(certPEM)
	if err != nil {
		return nil, nil, nil, err
	}
	leaf := chain[0]
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return nil, nil, nil, err
	}
	if !keyMatches(leaf.PublicKey, key) {
		return nil, nil, nil, errors.New("certificate does not match the private key")
	}
	if now.Before(leaf.NotBefore) {
		return nil, nil, nil, fmt.Errorf("certificate is not valid yet (starts %s)", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if !leaf.NotAfter.After(now.Add(minRemaining)) {
		return nil, nil, nil, fmt.Errorf("certificate expires too soon (%s); it must be valid for more than 24h", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	if len(leaf.DNSNames) == 0 {
		return nil, nil, nil, errors.New("certificate has no DNS subject alternative names")
	}
	seen := map[string]bool{}
	var names []string
	for _, n := range leaf.DNSNames {
		n = strings.ToLower(n)
		if err := ValidName(n); err != nil {
			return nil, nil, nil, err
		}
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return chain, key, names, nil
}

// ImportNames validates a certificate and key as [Import] does, without
// storing anything, and returns the names the certificate would cover.
func ImportNames(certPEM, keyPEM []byte, now time.Time) ([]string, error) {
	_, _, names, err := checkImport(certPEM, keyPEM, now)
	return names, err
}

// Import validates a bring-your-own certificate chain and private key and
// stores them as <dir>/certs/imported/<id>.{crt,key} (0600). The key may be
// RSA, ECDSA or Ed25519 (PKCS#1, PKCS#8 or SEC1). Importing the same names
// again replaces the previous files.
func Import(dir string, certPEM, keyPEM []byte, server bool, now time.Time) (Imported, error) {
	chain, key, names, err := checkImport(certPEM, keyPEM, now)
	if err != nil {
		return Imported{}, err
	}
	leaf := chain[0]
	id := ImportedID(names)

	var crt bytes.Buffer
	for _, c := range chain {
		_ = pem.Encode(&crt, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return Imported{}, err
	}
	keyOut := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	d := filepath.Join(dir, "certs", importedDir)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return Imported{}, err
	}
	// Key first, so the proxy never sees a certificate without its key.
	if err := writeAtomic(filepath.Join(d, id+".key"), keyOut, 0o600); err != nil {
		return Imported{}, err
	}
	if err := writeAtomic(filepath.Join(d, id+".crt"), crt.Bytes(), 0o600); err != nil {
		return Imported{}, err
	}
	marker := filepath.Join(d, id+serverExt)
	if server {
		if err := writeAtomic(marker, nil, 0o600); err != nil {
			return Imported{}, err
		}
	} else if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Imported{}, err
	}
	return Imported{ID: id, Names: names, NotAfter: leaf.NotAfter, Server: server}, nil
}

// RemoveImported deletes the imported certificate with the given id. It
// reports an error when there is none.
func RemoveImported(dir, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid certificate id %q", id)
	}
	d := filepath.Join(dir, "certs", importedDir)
	if _, err := os.Stat(filepath.Join(d, id+".crt")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no imported certificate %s", id)
		}
		return err
	}
	// Certificate first: without it the entry is gone from the listing.
	for _, ext := range []string{".crt", ".key", serverExt} {
		if err := os.Remove(filepath.Join(d, id+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ListImported returns the imported certificates sorted by id. Unparseable
// entries are skipped.
func ListImported(dir string) ([]Imported, error) {
	d := filepath.Join(dir, "certs", importedDir)
	entries, err := os.ReadDir(d)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Imported
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".crt")
		if !ok || e.IsDir() || !ValidID(id) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			return nil, err
		}
		chain, err := parseChain(data)
		if err != nil {
			continue
		}
		names := make([]string, 0, len(chain[0].DNSNames))
		for _, n := range chain[0].DNSNames {
			names = append(names, strings.ToLower(n))
		}
		sort.Strings(names)
		_, serr := os.Stat(filepath.Join(d, id+serverExt))
		out = append(out, Imported{ID: id, Names: names, NotAfter: chain[0].NotAfter, Server: serr == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func parsePrivateKey(data []byte) (crypto.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM private key found")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return nil, errors.New("unsupported private key (expected PKCS#1, PKCS#8 or EC)")
}

func keyMatches(pub any, key crypto.PrivateKey) bool {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		p, ok := pub.(*rsa.PublicKey)
		return ok && p.Equal(&k.PublicKey)
	case *ecdsa.PrivateKey:
		p, ok := pub.(*ecdsa.PublicKey)
		return ok && p.Equal(&k.PublicKey)
	case ed25519.PrivateKey:
		p, ok := pub.(ed25519.PublicKey)
		return ok && p.Equal(k.Public())
	}
	return false
}
