package tunnel

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const maxTokenLen = 4096

// errInvalidToken never includes the token.
var errInvalidToken = errors.New("tunnel token must be 1 to 4096 base64 characters")

// CleanToken trims surrounding whitespace and validates the charset of a
// tunnel token (the base64 token Cloudflare shows for a tunnel).
func CleanToken(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > maxTokenLen {
		return "", errInvalidToken
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '+', r == '/', r == '=', r == '-', r == '_':
		default:
			return "", errInvalidToken
		}
	}
	return s, nil
}

// secretDir is the directory mounted into the connector: 0700, holding the
// token file (0600) only.
func secretDir(dir string) string { return filepath.Join(dir, "secret") }
func tokenPath(dir string) string { return filepath.Join(secretDir(dir), "token") }
func genPath(dir string) string   { return filepath.Join(dir, "generation") }

// ensureDirs creates the tunnel directory (0755, so the compose file is
// readable) and the secret directory (0700).
func ensureDirs(dir string) error {
	for _, d := range []struct {
		p    string
		mode os.FileMode
	}{{dir, 0o755}, {secretDir(dir), 0o700}} {
		if err := os.MkdirAll(d.p, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.p, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// writeAtomic replaces path with data. The temp file gets its final mode
// before any data is written and is renamed over path, so a secret is never
// readable by others.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// storeToken writes the token file and a fresh generation.
func storeToken(dir, token string) error {
	if err := ensureDirs(dir); err != nil {
		return err
	}
	if err := writeAtomic(tokenPath(dir), []byte(token), 0o600); err != nil {
		return err
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	return writeAtomic(genPath(dir), []byte(hex.EncodeToString(b[:])), 0o644)
}

// removeToken deletes the token file and the generation.
func removeToken(dir string) error {
	for _, p := range []string{tokenPath(dir), genPath(dir)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func hasToken(dir string) bool {
	fi, err := os.Stat(tokenPath(dir))
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

func generation(dir string) string {
	b, err := os.ReadFile(genPath(dir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
