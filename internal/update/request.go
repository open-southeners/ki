package update

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// RequestFile and ResultFile are the names of the exchange files inside
	// the update directory.
	RequestFile = "request"
	ResultFile  = "result"

	// maxRequestSize bounds the request file root is willing to read.
	maxRequestSize = 4 << 10
	// maxResultSize bounds the result file the agent is willing to read.
	maxResultSize = 64 << 10
)

// ValidVersion reports whether v is an acceptable release tag.
func ValidVersion(v string) bool { return ValidTag(v) }

// Request asks the root helper to install a release. The agent writes it; the
// helper reads and deletes it.
type Request struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	RequestedBy string    `json:"requested_by"`
	RequestedAt time.Time `json:"requested_at"`
}

// WriteRequest writes the request file in dir atomically (temp file + rename)
// with mode 0640. It is called by the unprivileged agent.
func WriteRequest(dir string, r Request) error {
	if !ValidVersion(r.Version) {
		return fmt.Errorf("invalid version %q", r.Version)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > maxRequestSize {
		return errors.New("update request is too large")
	}
	return writeAtomic(dir, RequestFile, b, -1, -1)
}

// ReadRequest reads and deletes the request file in dir. It is called by root,
// and dir is writable by the unprivileged agent, so it never follows a
// symlink, accepts only a small regular file and parses strictly. The file is
// removed as soon as it has been read, whether or not it is valid.
func ReadRequest(dir string) (Request, error) {
	var r Request
	path := filepath.Join(dir, RequestFile)
	// O_NONBLOCK keeps a planted FIFO from blocking the open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			_ = os.Remove(path)
			return r, errors.New("update request is a symlink")
		}
		return r, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return r, err
	}
	if !st.Mode().IsRegular() {
		_ = os.Remove(path)
		return r, errors.New("update request is not a regular file")
	}
	if st.Size() > maxRequestSize {
		_ = os.Remove(path)
		return r, errors.New("update request is too large")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRequestSize+1))
	_ = os.Remove(path)
	if err != nil {
		return r, err
	}
	if len(b) > maxRequestSize {
		return r, errors.New("update request is too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Request{}, fmt.Errorf("decode update request: %w", err)
	}
	if dec.More() {
		return Request{}, errors.New("decode update request: trailing data")
	}
	if !ValidVersion(r.Version) {
		return Request{}, fmt.Errorf("invalid version %q in update request", r.Version)
	}
	return r, nil
}

// WriteResult writes the result file in dir as root: a new temp file created
// with O_EXCL|O_NOFOLLOW, renamed over result, owned by uid:gid with mode 0640.
// A negative uid or gid leaves that owner unchanged.
func WriteResult(dir string, res Result, uid, gid int) error {
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return writeAtomic(dir, ResultFile, b, uid, gid)
}

// ReadResult reads the result file in dir. The error satisfies
// errors.Is(err, os.ErrNotExist) when there is none.
func ReadResult(dir string) (Result, error) {
	var res Result
	f, err := os.OpenFile(filepath.Join(dir, ResultFile), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return res, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return res, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxResultSize {
		return res, errors.New("update result is not a small regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxResultSize))
	if err != nil {
		return res, err
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return Result{}, fmt.Errorf("decode update result: %w", err)
	}
	return res, nil
}

// writeAtomic creates a fresh temp file in dir (never reusing or following an
// existing name), writes b, sets ownership and 0640, and renames it to name.
func writeAtomic(dir, name string, b []byte, uid, gid int) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+"."+hex.EncodeToString(rnd[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o640)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err := f.Chown(uid, gid); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Chmod(0o640); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return err
	}
	ok = true
	return nil
}
