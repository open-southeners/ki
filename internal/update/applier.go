package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"

	"github.com/open-southeners/ki-agent/internal/client"
)

const (
	// DefaultBaseURL is where release assets are downloaded from.
	DefaultBaseURL = "https://github.com/open-southeners/ki-agent/releases/download"
	// DefaultBackupDir holds the previous binary and database backups. It is
	// root-only.
	DefaultBackupDir = "/var/lib/ki-agent-update"

	serviceName = "ki-agent"
	binaryName  = "ki-agent"

	maxArchiveSize   = 128 << 20
	maxBinarySize    = 256 << 20
	maxChecksumsSize = 1 << 20
	maxSigSize       = 4 << 10
	downloadTimeout  = 5 * time.Minute

	defaultHealthTimeout  = 60 * time.Second
	defaultHealthInterval = time.Second
	versionCmdTimeout     = 15 * time.Second
	serviceCmdTimeout     = 2 * time.Minute
)

// dbFiles are the SQLite files backed up and restored around a swap.
var dbFiles = []string{"state.db", "state.db-wal", "state.db-shm"}

// ServiceController stops and starts the agent's service.
type ServiceController interface {
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// HealthChecker makes one attempt to confirm the running agent is up and
// reports the wanted version. The applier retries it until its deadline.
type HealthChecker interface {
	Check(ctx context.Context, wantVersion string) error
}

// SystemctlService controls ki-agent.service with systemctl.
type SystemctlService struct{}

// Stop runs `systemctl stop ki-agent`.
func (SystemctlService) Stop(ctx context.Context) error { return systemctl(ctx, "stop") }

// Start runs `systemctl start ki-agent`.
func (SystemctlService) Start(ctx context.Context) error { return systemctl(ctx, "start") }

func systemctl(ctx context.Context, verb string) error {
	ctx, cancel := context.WithTimeout(ctx, serviceCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", verb, serviceName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", verb, serviceName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SocketHealth checks the agent over its local Unix socket: agent.hello (done
// by client.Dial) then agent.info, whose version must match.
type SocketHealth struct {
	SocketPath string
}

// Check implements [HealthChecker].
func (h SocketHealth) Check(ctx context.Context, wantVersion string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, h.SocketPath)
	if err != nil {
		return err
	}
	defer c.Close()
	var info struct {
		Version string `json:"version"`
	}
	if err := c.Call(ctx, "agent.info", nil, &info); err != nil {
		return err
	}
	if !sameVersion(info.Version, wantVersion) {
		return fmt.Errorf("agent reports version %q, want %q", info.Version, wantVersion)
	}
	return nil
}

// sameVersion reports whether two versions are equal once normalised by
// [Canonical], so a build reporting "1.0.2" matches the tag "v1.0.2".
func sameVersion(a, b string) bool { return Canonical(a) == Canonical(b) }

// Applier downloads, verifies and installs a release. It must run as root. The
// zero value of an optional field selects its default (see [Applier.Apply]).
type Applier struct {
	// BaseURL is the release download base URL; never taken from a request.
	BaseURL string
	// HTTP is the client for downloads.
	HTTP *http.Client
	// Keys are the trusted release signing keys; nil selects [ReleaseKeys].
	Keys map[string]ed25519.PublicKey
	// Current is the running version.
	Current string
	// Target is the binary to replace; empty selects BinDir/ki when it exists,
	// else the running executable with symlinks resolved.
	Target string
	// BinDir is where ki is installed; empty selects [DefaultBinDir].
	BinDir string
	// BackupDir holds ki-agent.previous and the database backups.
	BackupDir string
	// DataDir holds state.db and its WAL files.
	DataDir string
	Service ServiceController
	Health  HealthChecker
	Log     *slog.Logger
	// GOARCH selects the release archive; empty selects runtime.GOARCH.
	GOARCH string
	// HealthTimeout and HealthInterval bound the post-start health check.
	HealthTimeout  time.Duration
	HealthInterval time.Duration
}

// backedUp records a database file saved before the swap.
type backedUp struct {
	name     string
	mode     os.FileMode
	uid, gid int
}

func (a *Applier) logger() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// prepare fills in defaults.
func (a *Applier) prepare() error {
	if a.BaseURL == "" {
		a.BaseURL = DefaultBaseURL
	}
	if a.HTTP == nil {
		a.HTTP = &http.Client{}
	}
	if a.Keys == nil {
		a.Keys = ReleaseKeys()
	}
	if a.BackupDir == "" {
		a.BackupDir = DefaultBackupDir
	}
	if a.GOARCH == "" {
		a.GOARCH = runtime.GOARCH
	}
	if a.HealthTimeout <= 0 {
		a.HealthTimeout = defaultHealthTimeout
	}
	if a.HealthInterval <= 0 {
		a.HealthInterval = defaultHealthInterval
	}
	if a.Service == nil {
		a.Service = SystemctlService{}
	}
	if a.BinDir == "" {
		a.BinDir = DefaultBinDir
	}
	if a.Target == "" {
		if t, err := filepath.EvalSymlinks(filepath.Join(a.BinDir, cliName)); err == nil {
			a.Target = t
		} else {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate running binary: %w", err)
			}
			if a.Target, err = filepath.EvalSymlinks(exe); err != nil {
				return fmt.Errorf("resolve running binary: %w", err)
			}
		}
	}
	if a.Health == nil {
		return errors.New("no health checker configured")
	}
	if a.DataDir == "" {
		return errors.New("no data directory configured")
	}
	return nil
}

// Apply installs release version. The returned Result is always filled in and
// is what the caller records; the error is non-nil unless the status is
// "succeeded". A "failed" result means the swap never started (the service was
// not stopped, or was restarted untouched); "rolled_back" means the previous
// binary and database were restored.
func (a *Applier) Apply(ctx context.Context, version string) (Result, error) {
	res := Result{ID: newID(), To: version, StartedAt: time.Now().UTC()}
	res.From = a.Current
	finish := func(status string, err error) (Result, error) {
		res.Status = status
		if err != nil {
			res.Error = err.Error()
		}
		res.FinishedAt = time.Now().UTC()
		return res, err
	}
	if err := a.prepare(); err != nil {
		return finish(ResultFailed, err)
	}
	if !ValidVersion(version) {
		return finish(ResultFailed, fmt.Errorf("invalid version %q", version))
	}
	if cur := Canonical(a.Current); ValidTag(cur) && semver.Compare(version, cur) <= 0 {
		return finish(ResultFailed, fmt.Errorf("%s is not newer than the running version %s", version, a.Current))
	}

	if err := os.MkdirAll(a.BackupDir, 0o700); err != nil {
		return finish(ResultFailed, err)
	}
	work, err := os.MkdirTemp(a.BackupDir, "work-")
	if err != nil {
		return finish(ResultFailed, err)
	}
	defer os.RemoveAll(work)

	newBin, err := a.fetchAndVerify(ctx, version, work)
	if err != nil {
		return finish(ResultFailed, err)
	}

	log := a.logger()
	log.Info("update: stopping service", "from", a.Current, "to", version)
	if err := a.Service.Stop(ctx); err != nil {
		// It may have stopped anyway; make sure it runs again.
		_ = a.Service.Start(ctx)
		return finish(ResultFailed, err)
	}
	dbs, err := a.backupDB()
	if err != nil {
		a.restart(ctx)
		return finish(ResultFailed, fmt.Errorf("back up database: %w", err))
	}
	prev := filepath.Join(a.BackupDir, binaryName+".previous")
	if err := copyFile(a.Target, prev, 0o755, -1, -1); err != nil {
		a.restart(ctx)
		return finish(ResultFailed, fmt.Errorf("back up current binary: %w", err))
	}
	if err := installBinary(newBin, a.Target); err != nil {
		a.restart(ctx)
		return finish(ResultFailed, fmt.Errorf("install new binary: %w", err))
	}

	log.Info("update: starting service", "version", version)
	err = a.Service.Start(ctx)
	if err == nil {
		err = a.waitHealthy(ctx, version)
	}
	if err == nil {
		log.Info("update: succeeded", "version", version)
		return finish(ResultSucceeded, nil)
	}

	log.Error("update: new version did not come up; rolling back", "err", err)
	reason := err.Error()
	if rbErr := a.rollback(prev, dbs); rbErr != nil {
		reason += "; rollback failed: " + rbErr.Error()
	}
	return finish(ResultRolledBack, errors.New(reason))
}

// restart starts the service again after a failure before the swap.
func (a *Applier) restart(ctx context.Context) {
	if err := a.Service.Start(ctx); err != nil {
		a.logger().Error("update: restarting service", "err", err)
	}
}

func (a *Applier) waitHealthy(ctx context.Context, version string) error {
	deadline := time.Now().Add(a.HealthTimeout)
	var last error
	for {
		last = a.Health.Check(ctx, version)
		if last == nil {
			return nil
		}
		if time.Now().Add(a.HealthInterval).After(deadline) {
			return fmt.Errorf("health check failed: %w", last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.HealthInterval):
		}
	}
}

// rollback stops the service, restores the binary and database files and
// starts the service again. It uses a context that outlives cancellation of the
// caller's so an interrupted update still restores.
func (a *Applier) rollback(prev string, dbs []backedUp) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var errs []error
	if err := a.Service.Stop(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := installBinary(prev, a.Target); err != nil {
		errs = append(errs, fmt.Errorf("restore binary: %w", err))
	}
	if err := a.restoreDB(dbs); err != nil {
		errs = append(errs, fmt.Errorf("restore database: %w", err))
	}
	if err := a.Service.Start(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// fetchAndVerify downloads the release into work and returns the path of the
// verified, tag-checked new binary.
func (a *Applier) fetchAndVerify(ctx context.Context, tag, work string) (string, error) {
	archiveName := "ki-agent_linux_" + a.GOARCH + ".tar.gz"
	base := strings.TrimRight(a.BaseURL, "/") + "/" + tag + "/"
	checksums, err := a.download(ctx, base+"checksums.txt", maxChecksumsSize)
	if err != nil {
		return "", err
	}
	sig, err := a.download(ctx, base+"checksums.txt.sig", maxSigSize)
	if err != nil {
		return "", err
	}
	if err := Verify(a.Keys, tag, checksums, sig); err != nil {
		return "", fmt.Errorf("verify release signature: %w", err)
	}
	want, err := checksumFor(checksums, archiveName)
	if err != nil {
		return "", err
	}
	archive, err := a.download(ctx, base+archiveName, maxArchiveSize)
	if err != nil {
		return "", err
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return "", fmt.Errorf("checksum mismatch for %s", archiveName)
	}
	newBin := filepath.Join(work, binaryName)
	if err := extractBinary(archive, newBin); err != nil {
		return "", err
	}
	if err := checkVersion(ctx, newBin, tag); err != nil {
		return "", err
	}
	return newBin, nil
}

// download fetches url, failing on a non-200 status or a body over limit bytes.
func (a *Applier) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than %d bytes", url, limit)
	}
	return b, nil
}

// checksumFor finds name in a sha256sum-format checksums file.
func checksumFor(checksums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0]), nil
		}
	}
	return "", fmt.Errorf("%s is not listed in checksums.txt", name)
}

// errNoEntry means the archive has no entry of the wanted name.
var errNoEntry = errors.New("no such archive entry")

// extractBinary writes the archive's ki entry to dst (0700), falling back to
// ki-agent for releases that predate the rename. Only that one regular entry is
// read; anything else in the archive is ignored.
func extractBinary(archive []byte, dst string) error {
	err := extractEntry(archive, cliName, dst)
	if errors.Is(err, errNoEntry) {
		err = extractEntry(archive, binaryName, dst)
	}
	if errors.Is(err, errNoEntry) {
		return fmt.Errorf("archive has no %s or %s entry", cliName, binaryName)
	}
	return err
}

// extractEntry writes the regular archive entry called name to dst, or returns
// [errNoEntry].
func extractEntry(archive []byte, name, dst string) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errNoEntry
		}
		if err != nil {
			return fmt.Errorf("read archive: %w", err)
		}
		if path.Clean(h.Name) != name {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			return fmt.Errorf("archive entry %s is not a regular file", name)
		}
		if h.Size <= 0 || h.Size > maxBinarySize {
			return fmt.Errorf("archive entry %s has unreasonable size %d", name, h.Size)
		}
		f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o700)
		if err != nil {
			return err
		}
		n, err := io.Copy(f, io.LimitReader(tr, maxBinarySize+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		if n != h.Size {
			return fmt.Errorf("extract %s: size mismatch", name)
		}
		return nil
	}
}

// checkVersion runs `<bin> version` and requires the tag in its output.
func checkVersion(ctx context.Context, bin, tag string) error {
	ctx, cancel := context.WithTimeout(ctx, versionCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("run new binary: %w", err)
	}
	if !bytes.Contains(out, []byte(strings.TrimPrefix(tag, "v"))) {
		return fmt.Errorf("new binary does not report version %s", tag)
	}
	return nil
}

// backupDB copies the database files into the backup dir. The service must be
// stopped. Only regular files are copied; a stale backup of a file that no
// longer exists is removed.
func (a *Applier) backupDB() ([]backedUp, error) {
	var saved []backedUp
	for _, name := range dbFiles {
		src := filepath.Join(a.DataDir, name)
		dst := filepath.Join(a.BackupDir, name)
		st, err := os.Lstat(src)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", src)
		}
		b := backedUp{name: name, mode: st.Mode().Perm(), uid: -1, gid: -1}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			b.uid, b.gid = int(sys.Uid), int(sys.Gid)
		}
		if err := copyFile(src, dst, 0o600, -1, -1); err != nil {
			return nil, err
		}
		saved = append(saved, b)
	}
	return saved, nil
}

// restoreDB puts the backed-up files back (temp file + rename, original owner
// and mode) and removes database files that did not exist at backup time so a
// stale WAL cannot be applied to the restored database. The service must be
// stopped.
func (a *Applier) restoreDB(saved []backedUp) error {
	have := make(map[string]backedUp, len(saved))
	for _, b := range saved {
		have[b.name] = b
	}
	var errs []error
	for _, name := range dbFiles {
		dst := filepath.Join(a.DataDir, name)
		b, ok := have[name]
		if !ok {
			if st, err := os.Lstat(dst); err == nil && st.Mode().IsRegular() {
				if err := os.Remove(dst); err != nil {
					errs = append(errs, err)
				}
			}
			continue
		}
		if err := copyFile(filepath.Join(a.BackupDir, name), dst, b.mode, b.uid, b.gid); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// installBinary copies src next to target as .ki-agent.new (0755) and renames
// it over target.
func installBinary(src, target string) error {
	tmp := filepath.Join(filepath.Dir(target), ".ki-agent.new")
	if err := copyFileTo(src, tmp, 0o755, -1, -1); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// copyFile copies the regular file src to dst through a temp file in dst's
// directory and a rename. A negative uid or gid leaves that owner as is.
func copyFile(src, dst string, mode os.FileMode, uid, gid int) error {
	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := dst + ".tmp-" + hex.EncodeToString(rnd[:])
	if err := copyFileTo(src, tmp, mode, uid, gid); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// copyFileTo copies the regular file src to a newly created dst. An existing
// dst is removed first, never followed.
func copyFileTo(src, dst string, mode os.FileMode, uid, gid int) error {
	in, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer in.Close()
	if st, err := in.Stat(); err != nil {
		return err
	} else if !st.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		return fail(err)
	}
	if uid >= 0 || gid >= 0 {
		if err := out.Chown(uid, gid); err != nil {
			return fail(err)
		}
	}
	if err := out.Chmod(mode); err != nil {
		return fail(err)
	}
	if err := out.Sync(); err != nil {
		return fail(err)
	}
	return out.Close()
}

// Values of [Result.Status].
const (
	ResultSucceeded  = "succeeded"
	ResultFailed     = "failed"
	ResultRolledBack = "rolled_back"
)

// newID returns a random identifier for results not tied to a request; callers
// handling a request overwrite Result.ID with the request's id.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
