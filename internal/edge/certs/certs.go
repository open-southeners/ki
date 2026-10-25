// Package certs manages the edge's TLS material: the persistent self-signed
// default certificate and control-plane-issued certificates (per-host key,
// CSR, installed chain). Standard library crypto only.
package certs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Cert describes the self-signed default certificate.
type Cert struct {
	CertFile          string
	KeyFile           string
	FingerprintSHA256 string
	NotAfter          time.Time
}

// Installed describes an installed control-plane certificate.
type Installed struct {
	Hostname string
	NotAfter time.Time
}

// InterfaceLister returns the addresses of the host's network interfaces.
type InterfaceLister func() ([]net.Addr, error)

const (
	selfSignedValidity = 10 * 365 * 24 * time.Hour
	minRemaining       = 24 * time.Hour
)

// ValidHostname checks that name is a lowercase ASCII DNS name usable both as
// a domain and as a file name. Internationalised names must be punycode.
func ValidHostname(name string) error {
	if name == "" {
		return errors.New("hostname is empty")
	}
	if len(name) > 253 {
		return errors.New("hostname is longer than 253 characters")
	}
	for i := 0; i < len(name); i++ {
		if name[i] >= 0x80 {
			return errors.New("hostname must be ASCII (use punycode for internationalised names)")
		}
	}
	if strings.Contains(name, "*") {
		return errors.New("wildcard hostnames are not supported")
	}
	if net.ParseIP(name) != nil {
		return errors.New("hostname must be a DNS name, not an IP address")
	}
	if !strings.Contains(name, ".") {
		return errors.New("hostname must contain at least one dot")
	}
	labels := strings.Split(name, ".")
	if last := labels[len(labels)-1]; strings.Trim(last, "0123456789") == "" {
		return fmt.Errorf("hostname %q: the last label cannot be numeric", name)
	}
	for _, label := range labels {
		if label == "" {
			return errors.New("hostname has an empty label")
		}
		if len(label) > 63 {
			return fmt.Errorf("hostname label %q is longer than 63 characters", label)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("hostname label %q must not start or end with '-'", label)
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return fmt.Errorf("hostname contains invalid character %q (must be lowercase a-z, 0-9, '-')", string(c))
			}
		}
	}
	return nil
}

// LoadOrCreateSelfSigned returns the persistent self-signed certificate in
// dir, generating it on first use.
func LoadOrCreateSelfSigned(dir string) (Cert, error) {
	return loadOrCreateSelfSigned(dir, net.InterfaceAddrs, os.Hostname)
}

func loadOrCreateSelfSigned(dir string, lister InterfaceLister, hostname func() (string, error)) (Cert, error) {
	c := Cert{
		CertFile: filepath.Join(dir, "selfsigned.crt"),
		KeyFile:  filepath.Join(dir, "selfsigned.key"),
	}
	_, certErr := os.Stat(c.CertFile)
	_, keyErr := os.Stat(c.KeyFile)
	for _, e := range []error{certErr, keyErr} {
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return Cert{}, e
		}
	}
	certExists, keyExists := certErr == nil, keyErr == nil
	switch {
	case certExists && keyExists:
		leaf, err := loadPair(c.CertFile, c.KeyFile)
		if err != nil {
			return Cert{}, fmt.Errorf("existing self-signed certificate: %w", err)
		}
		c.FingerprintSHA256 = Fingerprint(leaf.Raw)
		c.NotAfter = leaf.NotAfter
		return c, nil
	case certExists != keyExists:
		return Cert{}, fmt.Errorf("self-signed certificate and key are inconsistent (only one of %s, %s exists); refusing to overwrite", c.CertFile, c.KeyFile)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Cert{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Cert{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Cert{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ki-agent"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	if h, err := hostname(); err == nil && h != "" {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	if addrs, err := lister(); err == nil {
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			dup := false
			for _, e := range tmpl.IPAddresses {
				if e.Equal(ip) {
					dup = true
				}
			}
			if !dup {
				tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
			}
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return Cert{}, err
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return Cert{}, err
	}
	// Key first: a crash between the writes leaves a lone key, which errors
	// loudly instead of silently minting a different identity.
	if err := writeAtomic(c.KeyFile, keyPEM, 0o600); err != nil {
		return Cert{}, err
	}
	if err := writeAtomic(c.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return Cert{}, err
	}
	c.FingerprintSHA256 = Fingerprint(der)
	c.NotAfter = tmpl.NotAfter.Truncate(time.Second).UTC()
	return c, nil
}

// Fingerprint formats the SHA-256 of a DER certificate as uppercase hex with
// colons.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	parts := make([]string, 0, len(sum))
	for i := 0; i < len(h); i += 2 {
		parts = append(parts, h[i:i+2])
	}
	return strings.Join(parts, ":")
}

// CSR returns a PEM certificate request for hostname, creating the key under
// <dir>/certs/<hostname>.key unless one already exists.
func CSR(dir, hostname string) ([]byte, error) {
	if err := ValidHostname(hostname); err != nil {
		return nil, err
	}
	certDir := filepath.Join(dir, "certs")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return nil, err
	}
	keyFile := filepath.Join(certDir, hostname+".key")
	key, err := readKey(keyFile)
	if errors.Is(err, os.ErrNotExist) {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		keyPEM, merr := marshalKey(key)
		if merr != nil {
			return nil, merr
		}
		if err := writeAtomic(keyFile, keyPEM, 0o600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: hostname},
		DNSNames: []string{hostname},
	}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// Install validates pemChain against the stored key and writes it as
// <dir>/certs/<hostname>.crt. The chain need not verify to system roots.
func Install(dir, hostname string, pemChain []byte, now time.Time) (Installed, error) {
	if err := ValidHostname(hostname); err != nil {
		return Installed{}, err
	}
	chain, err := parseChain(pemChain)
	if err != nil {
		return Installed{}, err
	}
	leaf := chain[0]
	certDir := filepath.Join(dir, "certs")
	key, err := readKey(filepath.Join(certDir, hostname+".key"))
	if errors.Is(err, os.ErrNotExist) {
		return Installed{}, fmt.Errorf("no pending key for %s; request a CSR first", hostname)
	} else if err != nil {
		return Installed{}, err
	}
	if !publicKeyMatches(leaf.PublicKey, key) {
		return Installed{}, errors.New("certificate does not match the stored private key")
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return Installed{}, fmt.Errorf("certificate does not cover %s", hostname)
	}
	if now.Before(leaf.NotBefore) {
		return Installed{}, fmt.Errorf("certificate is not valid yet (starts %s)", leaf.NotBefore.UTC().Format(time.RFC3339))
	}
	if !leaf.NotAfter.After(now.Add(minRemaining)) {
		return Installed{}, fmt.Errorf("certificate expires too soon (%s); it must be valid for more than 24h", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	var buf bytes.Buffer
	for _, c := range chain {
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
	}
	if err := writeAtomic(filepath.Join(certDir, hostname+".crt"), buf.Bytes(), 0o644); err != nil {
		return Installed{}, err
	}
	return Installed{Hostname: hostname, NotAfter: leaf.NotAfter}, nil
}

// Remove deletes the installed certificate and key for hostname. Missing
// files are not an error.
func Remove(dir, hostname string) error {
	if err := ValidHostname(hostname); err != nil {
		return err
	}
	certDir := filepath.Join(dir, "certs")
	for _, ext := range []string{".crt", ".key"} {
		if err := os.Remove(filepath.Join(certDir, hostname+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// List returns installed certificates (those with a .crt), sorted by
// hostname. Unparseable entries are skipped.
func List(dir string) ([]Installed, error) {
	certDir := filepath.Join(dir, "certs")
	entries, err := os.ReadDir(certDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var out []Installed
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".crt")
		if !ok || e.IsDir() || ValidHostname(name) != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(certDir, e.Name()))
		if err != nil {
			return nil, err
		}
		chain, err := parseChain(data)
		if err != nil {
			continue
		}
		out = append(out, Installed{Hostname: name, NotAfter: chain[0].NotAfter})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Hostname < out[j].Hostname })
	return out, nil
}

func parseChain(data []byte) ([]*x509.Certificate, error) {
	var chain []*x509.Certificate
	rest := data
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("unexpected PEM block %q in certificate chain", block.Type)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("invalid certificate: %w", err)
		}
		chain = append(chain, c)
	}
	if len(chain) == 0 {
		return nil, errors.New("no PEM certificate found")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("trailing data after PEM certificates")
	}
	return chain, nil
}

func loadPair(certFile, keyFile string) (*x509.Certificate, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	chain, err := parseChain(data)
	if err != nil {
		return nil, err
	}
	key, err := readKey(keyFile)
	if err != nil {
		return nil, err
	}
	if !publicKeyMatches(chain[0].PublicKey, key) {
		return nil, errors.New("certificate does not match the private key")
	}
	return chain[0], nil
}

func publicKeyMatches(pub any, key *ecdsa.PrivateKey) bool {
	p, ok := pub.(*ecdsa.PublicKey)
	return ok && p.Equal(&key.PublicKey)
}

func readKey(path string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s: no PEM data", path)
	}
	k, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return k, nil
}

func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
