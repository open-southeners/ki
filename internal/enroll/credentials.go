package enroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// LoadCredentials reads the credentials file at path. A missing file yields an
// error satisfying errors.Is(err, fs.ErrNotExist). Files readable or writable
// by group or others are rejected, because they hold the control-plane token.
func LoadCredentials(path string) (*Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat credentials file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("credentials file %s is not a regular file", path)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("credentials file %s has mode %04o: want 0600 (no group/other access)", path, perm)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}
	if len(b) > maxBodyBytes {
		return nil, fmt.Errorf("credentials file %s is too large", path)
	}
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse credentials file %s: not valid credentials JSON", path)
	}
	if c.URL == "" || c.Token == "" {
		return nil, fmt.Errorf("credentials file %s is missing url or token", path)
	}
	return &c, nil
}

// SaveCredentials writes c to path with mode 0600, atomically: the data goes
// to a temporary file in the same directory which is then renamed over path.
func SaveCredentials(path string, c Credentials) (err error) {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".control-plane-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary credentials file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary credentials file: %w", err)
	}
	if _, err = tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credentials file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync credentials file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close credentials file: %w", err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("install credentials file: %w", err)
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// RemoveCredentials deletes the credentials file at path. A missing file
// yields an error satisfying errors.Is(err, fs.ErrNotExist).
func RemoveCredentials(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return err
		}
		return fmt.Errorf("remove credentials file: %w", err)
	}
	return nil
}
