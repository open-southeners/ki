package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testCurrent = "v0.1.0"
	testTarget  = "v0.2.0"
	oldBinary   = "#!/bin/sh\necho old\n"
)

type fakeService struct {
	events  []string
	onStart func(n int) // called with the number of starts so far, after recording
	starts  int
}

func (s *fakeService) Stop(context.Context) error {
	s.events = append(s.events, "stop")
	return nil
}

func (s *fakeService) Start(context.Context) error {
	s.starts++
	s.events = append(s.events, "start")
	if s.onStart != nil {
		s.onStart(s.starts)
	}
	return nil
}

type fakeHealth struct{ err error }

func (h fakeHealth) Check(context.Context, string) error { return h.err }

type fakeRelease struct {
	tag       string
	signedTag string // tag the signature binds to; defaults to tag
	script    string
	badSum    bool
	key       ed25519.PrivateKey
	archive   []byte
	checksums []byte
	sig       []byte
}

func fakeScript(tag string) string {
	return "#!/bin/sh\necho \"ki-agent " + tag + "\"\n"
}

func (r *fakeRelease) build(t *testing.T) {
	t.Helper()
	if r.signedTag == "" {
		r.signedTag = r.tag
	}
	if r.script == "" {
		r.script = fakeScript(r.tag)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// An unrelated entry first, to prove only ki-agent is picked.
	if err := tw.WriteHeader(&tar.Header{Name: "README.md", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("hi"))
	if err := tw.WriteHeader(&tar.Header{Name: "ki-agent", Mode: 0o755, Size: int64(len(r.script)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(r.script))
	_ = tw.Close()
	_ = gz.Close()
	r.archive = buf.Bytes()

	sum := sha256.Sum256(r.archive)
	hexSum := hex.EncodeToString(sum[:])
	if r.badSum {
		hexSum = strings.Repeat("0", 64)
	}
	r.checksums = []byte(hexSum + "  ki-agent_linux_amd64.tar.gz\n")
	pub := r.key.Public().(ed25519.PublicKey)
	r.sig = FormatSignature(KeyID(pub), ed25519.Sign(r.key, SignedMessage(r.signedTag, r.checksums)))
}

func (r *fakeRelease) handler() http.Handler {
	mux := http.NewServeMux()
	p := "/" + r.tag + "/"
	mux.HandleFunc(p+"ki-agent_linux_amd64.tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(r.archive) })
	mux.HandleFunc(p+"checksums.txt", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(r.checksums) })
	mux.HandleFunc(p+"checksums.txt.sig", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(r.sig) })
	return mux
}

type fixture struct {
	a       *Applier
	svc     *fakeService
	target  string
	dataDir string
	backup  string
}

func newFixture(t *testing.T, rel *fakeRelease, pub ed25519.PublicKey) *fixture {
	t.Helper()
	rel.build(t)
	srv := httptest.NewServer(rel.handler())
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	f := &fixture{
		svc:     &fakeService{},
		target:  filepath.Join(dir, "bin", "ki-agent"),
		dataDir: filepath.Join(dir, "data"),
		backup:  filepath.Join(dir, "backup"),
	}
	for _, d := range []string{filepath.Dir(f.target), f.dataDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, f.target, oldBinary, 0o755)
	mustWrite(t, filepath.Join(f.dataDir, "state.db"), "db-old", 0o600)
	mustWrite(t, filepath.Join(f.dataDir, "state.db-wal"), "wal-old", 0o600)
	// No state.db-shm on purpose: a file created by the new version must go.

	f.a = &Applier{
		BaseURL:        srv.URL,
		HTTP:           srv.Client(),
		Keys:           map[string]ed25519.PublicKey{KeyID(pub): pub},
		Current:        testCurrent,
		Target:         f.target,
		BackupDir:      f.backup,
		DataDir:        f.dataDir,
		Service:        f.svc,
		Health:         fakeHealth{},
		GOARCH:         "amd64",
		HealthTimeout:  200 * time.Millisecond,
		HealthInterval: 20 * time.Millisecond,
	}
	return f
}

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

// assertUntouched checks that nothing was swapped and the service never ran
// through a stop (pre-swap failures).
func assertUntouched(t *testing.T, f *fixture) {
	t.Helper()
	if got := mustRead(t, f.target); got != oldBinary {
		t.Errorf("binary changed: %q", got)
	}
	if len(f.svc.events) != 0 {
		t.Errorf("service was touched: %v", f.svc.events)
	}
}

func TestApplyHappyPath(t *testing.T) {
	pub, priv := newKey(t)
	f := newFixture(t, &fakeRelease{tag: testTarget, key: priv}, pub)
	res, err := f.a.Apply(context.Background(), testTarget)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Status != ResultSucceeded || res.From != testCurrent || res.To != testTarget || res.ID == "" {
		t.Errorf("result = %+v", res)
	}
	if got := mustRead(t, f.target); got != fakeScript(testTarget) {
		t.Errorf("binary = %q", got)
	}
	if st, _ := os.Stat(f.target); st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if got := mustRead(t, filepath.Join(f.backup, "ki-agent.previous")); got != oldBinary {
		t.Errorf("previous = %q", got)
	}
	if got := mustRead(t, filepath.Join(f.backup, "state.db")); got != "db-old" {
		t.Errorf("db backup = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(f.dataDir, "..", "bin", ".ki-agent.new")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temp binary left behind: %v", err)
	}
	if fmt.Sprint(f.svc.events) != "[stop start]" {
		t.Errorf("events = %v", f.svc.events)
	}
}

func TestApplyRejectsBadReleases(t *testing.T) {
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)
	cases := []struct {
		name    string
		rel     *fakeRelease
		wantErr string
	}{
		{"bad signature", &fakeRelease{tag: testTarget, key: otherPriv}, "signature"},
		{"signature for another tag", &fakeRelease{tag: testTarget, signedTag: "v0.1.5", key: priv}, "signature"},
		{"checksum mismatch", &fakeRelease{tag: testTarget, key: priv, badSum: true}, "checksum mismatch"},
		{"binary reports other version", &fakeRelease{tag: testTarget, key: priv, script: fakeScript("v9.9.9")}, "does not report"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, tc.rel, pub)
			res, err := f.a.Apply(context.Background(), testTarget)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
			if res.Status != ResultFailed || res.Error == "" {
				t.Errorf("result = %+v", res)
			}
			assertUntouched(t, f)
		})
	}
}

func TestApplyRefusesDowngrade(t *testing.T) {
	pub, priv := newKey(t)
	for _, tag := range []string{"v0.1.0", "v0.0.9"} {
		f := newFixture(t, &fakeRelease{tag: tag, key: priv}, pub)
		res, err := f.a.Apply(context.Background(), tag)
		if err == nil || res.Status != ResultFailed {
			t.Fatalf("%s: res = %+v, err = %v", tag, res, err)
		}
		assertUntouched(t, f)
	}
}

func TestApplyRejectsInvalidVersion(t *testing.T) {
	pub, priv := newKey(t)
	f := newFixture(t, &fakeRelease{tag: testTarget, key: priv}, pub)
	if _, err := f.a.Apply(context.Background(), "../v0.2.0"); err == nil {
		t.Fatal("expected error")
	}
	assertUntouched(t, f)
}

func TestApplyRollsBackOnHealthFailure(t *testing.T) {
	pub, priv := newKey(t)
	f := newFixture(t, &fakeRelease{tag: testTarget, key: priv}, pub)
	f.a.Health = fakeHealth{err: errors.New("no socket")}
	// The first start is the new version, which migrates the database and
	// leaves a shm file behind.
	f.svc.onStart = func(n int) {
		if n == 1 {
			mustWrite(t, filepath.Join(f.dataDir, "state.db"), "db-new", 0o600)
			mustWrite(t, filepath.Join(f.dataDir, "state.db-wal"), "wal-new", 0o600)
			mustWrite(t, filepath.Join(f.dataDir, "state.db-shm"), "shm-new", 0o600)
		}
	}
	res, err := f.a.Apply(context.Background(), testTarget)
	if err == nil {
		t.Fatal("expected error")
	}
	if res.Status != ResultRolledBack || !strings.Contains(res.Error, "no socket") {
		t.Errorf("result = %+v", res)
	}
	if got := mustRead(t, f.target); got != oldBinary {
		t.Errorf("binary = %q", got)
	}
	if got := mustRead(t, filepath.Join(f.dataDir, "state.db")); got != "db-old" {
		t.Errorf("state.db = %q", got)
	}
	if got := mustRead(t, filepath.Join(f.dataDir, "state.db-wal")); got != "wal-old" {
		t.Errorf("state.db-wal = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(f.dataDir, "state.db-shm")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("state.db-shm should be gone: %v", err)
	}
	if fmt.Sprint(f.svc.events) != "[stop start stop start]" {
		t.Errorf("events = %v", f.svc.events)
	}
}

func TestApplyRefusesNonRegularDB(t *testing.T) {
	pub, priv := newKey(t)
	f := newFixture(t, &fakeRelease{tag: testTarget, key: priv}, pub)
	victim := filepath.Join(t.TempDir(), "victim")
	mustWrite(t, victim, "secret", 0o600)
	wal := filepath.Join(f.dataDir, "state.db-wal")
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, wal); err != nil {
		t.Fatal(err)
	}
	res, err := f.a.Apply(context.Background(), testTarget)
	if err == nil || res.Status != ResultFailed {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if got := mustRead(t, f.target); got != oldBinary {
		t.Errorf("binary changed: %q", got)
	}
	// The service was stopped for the backup, so it must have been started again.
	if fmt.Sprint(f.svc.events) != "[stop start]" {
		t.Errorf("events = %v", f.svc.events)
	}
	if _, err := os.Lstat(filepath.Join(f.backup, "state.db-wal")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("symlink target was copied: %v", err)
	}
}

func TestExtractBinaryRejectsNonRegular(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "ki-agent", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink})
	_ = tw.Close()
	_ = gz.Close()
	if err := extractBinary(buf.Bytes(), filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("expected error for symlink entry")
	}
}

func TestDownloadSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 100))
	}))
	defer srv.Close()
	a := &Applier{HTTP: srv.Client()}
	if _, err := a.download(context.Background(), srv.URL, 50); err == nil {
		t.Fatal("expected size error")
	}
	if _, err := a.download(context.Background(), srv.URL, 100); err != nil {
		t.Fatal(err)
	}
}
