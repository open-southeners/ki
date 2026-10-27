// Command release-sign generates and uses the ed25519 release signing key.
//
//	release-sign keygen --out <file>
//	release-sign sign --tag <tag> --in <checksums.txt> --out <sig>
//
// keygen writes the private seed to a new file (0600) and prints the public
// half as a line for internal/update/keys.go. sign reads the seed from the
// KI_RELEASE_SIGNING_KEY environment variable (base64, 32 bytes) and writes
// the checksums.txt.sig file the updater verifies.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/open-southeners/ki/internal/update"
)

const keyEnv = "KI_RELEASE_SIGNING_KEY"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "release-sign:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer, getenv func(string) string) error {
	if len(args) == 0 {
		return errors.New("usage: release-sign keygen --out <file> | sign --tag <tag> --in <checksums.txt> --out <sig>")
	}
	switch args[0] {
	case "keygen":
		fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
		out := fs.String("out", "", "file to write the private seed to (must not exist)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *out == "" {
			return errors.New("keygen: --out is required")
		}
		return keygen(*out, stdout)
	case "sign":
		fs := flag.NewFlagSet("sign", flag.ContinueOnError)
		tag := fs.String("tag", "", "release tag, e.g. v1.2.3")
		in := fs.String("in", "", "checksums.txt to sign")
		out := fs.String("out", "", "signature file to write")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *tag == "" || *in == "" || *out == "" {
			return errors.New("sign: --tag, --in and --out are required")
		}
		return sign(*tag, *in, *out, getenv(keyEnv))
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func keygen(out string, stdout io.Writer) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(priv.Seed()) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(out)
		return fmt.Errorf("keygen: write %s: %w", out, werr)
	}

	id := update.KeyID(pub)
	var b strings.Builder
	for i, c := range pub {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "0x%02x", c)
	}
	fmt.Fprintf(stdout, "private seed written to %s (mode 0600); store it as the %s secret\n", out, keyEnv)
	fmt.Fprintf(stdout, "public key: %s\n", base64.StdEncoding.EncodeToString(pub))
	fmt.Fprintf(stdout, "key id:     %s\n", id)
	fmt.Fprintf(stdout, "add to releaseKeys in internal/update/keys.go:\n")
	fmt.Fprintf(stdout, "\t%q: ed25519.PublicKey{%s},\n", id, b.String())
	return nil
}

func sign(tag, in, out, seedB64 string) error {
	if seedB64 == "" {
		return fmt.Errorf("sign: %s is not set", keyEnv)
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(seedB64))
	if err != nil || len(seed) != ed25519.SeedSize {
		return fmt.Errorf("sign: %s is not a base64 %d-byte seed", keyEnv, ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	data, err := os.ReadFile(in)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	sig := ed25519.Sign(priv, update.SignedMessage(tag, data))
	file := update.FormatSignature(update.KeyID(pub), sig)
	if err := update.Verify(map[string]ed25519.PublicKey{update.KeyID(pub): pub}, tag, data, file); err != nil {
		return fmt.Errorf("sign: self-verification failed: %w", err)
	}
	if err := os.WriteFile(out, file, 0o644); err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	return nil
}
