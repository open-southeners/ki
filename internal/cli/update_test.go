package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/update"
	"github.com/open-southeners/ki-agent/internal/version"
)

const updStatusJSON = `{"current":"v0.3.0","channel":"stable","check_enabled":true,"checked_at":"2026-10-27T10:00:00Z",
"check_error":null,"latest":{"tag":"v0.4.0","name":"0.4.0","published_at":"2026-10-27T09:00:00Z","url":"https://example/r","notes":"n","prerelease":false},
"update_available":true,"apply_supported":true,"state":"idle","last_result":null}`

func updateStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		for _, m := range []string{update.MethodStatus, update.MethodCheck} {
			r.Handle(m, func(_ context.Context, _ *rpc.Peer, p json.RawMessage) (any, error) {
				rec.add(m, p)
				return json.RawMessage(updStatusJSON), nil
			})
		}
	}
}

// asUser makes the update command behave as a regular user for the test.
func asUser(t *testing.T) {
	t.Helper()
	old := updateGeteuid
	updateGeteuid = func() int { return 1000 }
	t.Cleanup(func() { updateGeteuid = old })
}

func TestUpdateAsUser(t *testing.T) {
	asUser(t)
	rec := &calls{}
	sock := startAgent(t, updateStubs(rec))

	out, _, err := runCLI(t, sock, "", "update")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"v0.3.0", "v0.4.0 (update available)", "stable", "Run `sudo ki agent update` to install v0.4.0."} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if rec.count(update.MethodStatus) != 1 || rec.count(update.MethodCheck) != 0 {
		t.Errorf("calls: status=%d check=%d", rec.count(update.MethodStatus), rec.count(update.MethodCheck))
	}

	if _, _, err := runCLI(t, sock, "", "update", "--check"); err != nil {
		t.Fatal(err)
	}
	if rec.count(update.MethodCheck) != 1 {
		t.Errorf("--check did not call %s", update.MethodCheck)
	}
}

func TestUpdateFromRequestNeedsRoot(t *testing.T) {
	asUser(t)
	_, _, err := runCLI(t, "/nonexistent.sock", "", "update", "--from-request")
	if err == nil || !strings.Contains(err.Error(), "must run as root") {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateFlags(t *testing.T) {
	cmd := newUpdateCmd(&flags{})
	if f := cmd.Flags().Lookup("from-request"); f == nil || !f.Hidden {
		t.Error("--from-request must exist and be hidden")
	}
	for _, name := range []string{"version", "yes", "check"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag --%s", name)
		}
	}
	if cmd.Flags().ShorthandLookup("y") == nil {
		t.Error("missing -y")
	}
}

// updRoot sets up the root path: a fake GitHub, a config with its own data
// directory and a running version, and returns the data dir.
func updRoot(t *testing.T, current string, latestTag string) (dataDir string, cfgPath string) {
	t.Helper()
	old := updateGeteuid
	updateGeteuid = func() int { return 0 }
	oldBase, oldApplier, oldVersion, oldLayout := updateAPIBaseURL, updateApplier, version.Version, updateLayout
	t.Cleanup(func() {
		updateGeteuid, updateAPIBaseURL, updateApplier, version.Version, updateLayout = old, oldBase, oldApplier, oldVersion, oldLayout
	})
	// Layout runs against temp dirs: a 1.0.x install with a single ki-agent.
	layoutBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(layoutBin, "ki-agent"), []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	updateLayout = func(o *update.LayoutOptions) {
		o.BinDir = layoutBin
		o.UnitDir = t.TempDir()
		o.DaemonReload = func(context.Context) error { return nil }
	}
	layoutDirs[t.Name()] = layoutBin
	version.Version = current

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			_, _ = w.Write([]byte(`{"tag_name":"` + latestTag + `","name":"` + latestTag + `","body":"line one\nline two","html_url":"https://example/r"}`))
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			tag := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_, _ = w.Write([]byte(`{"tag_name":"` + tag + `","body":"older"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(gh.Close)
	updateAPIBaseURL = gh.URL
	// Downloads go to a server that has nothing, so installing fails early.
	dl := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dl.Close)
	updateApplier = func(a *update.Applier) {
		a.BaseURL = dl.URL
		a.BackupDir = filepath.Join(t.TempDir(), "backup")
		a.Target = filepath.Join(t.TempDir(), "ki-agent")
		a.Service = noopService{}
	}

	dir := t.TempDir()
	dataDir = filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("data_dir: "+dataDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dataDir, cfgPath
}

// layoutDirs maps a test name to the bin dir its layout step works on.
var layoutDirs = map[string]string{}

func assertMigrated(t *testing.T, want bool) {
	t.Helper()
	st, err := os.Lstat(filepath.Join(layoutDirs[t.Name()], "ki"))
	if got := err == nil && st.Mode().IsRegular(); got != want {
		t.Errorf("ki installed = %v, want %v", got, want)
	}
}

type noopService struct{}

func (noopService) Stop(context.Context) error  { return nil }
func (noopService) Start(context.Context) error { return nil }

func runUpdateRootCLI(t *testing.T, cfgPath, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"agent", "--config", cfgPath, "--socket", filepath.Join(t.TempDir(), "none.sock")}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func TestUpdateRootCheckAndUpToDate(t *testing.T) {
	_, cfg := updRoot(t, "v0.3.0", "v0.4.0")
	out, err := runUpdateRootCLI(t, cfg, "", "update", "--check")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Update v0.3.0 -> v0.4.0", "line one", "line two", "https://example/r", "sudo ki agent update"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	assertMigrated(t, false) // --check must not touch the install

	_, cfg = updRoot(t, "v0.4.0", "v0.4.0")
	if out, err = runUpdateRootCLI(t, cfg, "", "update"); err != nil || !strings.Contains(out, "up to date") {
		t.Errorf("up to date: %q, %v", out, err)
	}
	if !strings.Contains(out, "layout: renamed ki-agent to ki") {
		t.Errorf("layout step did not report: %s", out)
	}
	assertMigrated(t, true)
	if _, err = runUpdateRootCLI(t, cfg, "", "update", "--version", "v0.3.0"); err == nil || !strings.Contains(err.Error(), "downgrades are refused") {
		t.Errorf("downgrade err = %v", err)
	}

	_, cfg = updRoot(t, "dev", "v0.4.0")
	if out, err = runUpdateRootCLI(t, cfg, "", "update"); err != nil || !strings.Contains(out, "source build") {
		t.Errorf("dev build: %q, %v", out, err)
	}
}

func TestUpdateRootConfirmation(t *testing.T) {
	_, cfg := updRoot(t, "v0.3.0", "v0.4.0")

	// Without a terminal and without --yes it refuses.
	old := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = old })
	stdinIsTerminal = func(io.Reader) bool { return false }
	if _, err := runUpdateRootCLI(t, cfg, "y\n", "update"); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("no tty err = %v", err)
	}

	// An answer other than yes cancels without installing.
	stdinIsTerminal = func(io.Reader) bool { return true }
	out, err := runUpdateRootCLI(t, cfg, "n\n", "update")
	if err != nil || !strings.Contains(out, "Cancelled.") {
		t.Errorf("decline: %q, %v", out, err)
	}
}

func TestUpdateRootFailureIsRecorded(t *testing.T) {
	dataDir, cfg := updRoot(t, "v0.3.0", "v0.4.0")
	_, err := runUpdateRootCLI(t, cfg, "", "update", "--yes")
	if err == nil || ExitCode(err) != 1 || !strings.Contains(err.Error(), "update failed") {
		t.Fatalf("err = %v (exit %d)", err, ExitCode(err))
	}
	res, rerr := update.ReadResult(filepath.Join(dataDir, "update"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if res.Status != update.ResultFailed || res.From != "v0.3.0" || res.To != "v0.4.0" || res.Error == "" {
		t.Errorf("result = %+v", res)
	}
}

func TestUpdateFromRequestRecordsResultUnderRequestID(t *testing.T) {
	dataDir, cfg := updRoot(t, "v0.3.0", "v0.4.0")
	dir := filepath.Join(dataDir, "update")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := update.WriteRequest(dir, update.Request{ID: "req-1", Version: "v0.4.0", RequestedBy: "ana"}); err != nil {
		t.Fatal(err)
	}
	_, err := runUpdateRootCLI(t, cfg, "", "update", "--from-request")
	if err == nil || ExitCode(err) != 1 {
		t.Fatalf("err = %v", err)
	}
	res, rerr := update.ReadResult(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if res.ID != "req-1" || res.Status != update.ResultFailed {
		t.Errorf("result = %+v", res)
	}
	if _, serr := os.Stat(filepath.Join(dir, update.RequestFile)); !errors.Is(serr, os.ErrNotExist) {
		t.Errorf("request file still there: %v", serr)
	}
	// With no request there is nothing to do.
	if _, err := runUpdateRootCLI(t, cfg, "", "update", "--from-request"); err == nil {
		t.Error("missing request accepted")
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(errors.New("x")) != 1 || ExitCode(&exitError{code: 2}) != 2 {
		t.Error("ExitCode mapping wrong")
	}
	err := outcome(update.Result{Status: update.ResultRolledBack, To: "v1.0.0", Error: "boom"}, errors.New("boom"))
	if ExitCode(err) != 2 {
		t.Errorf("rolled back exit = %d", ExitCode(err))
	}
	if outcome(update.Result{Status: update.ResultSucceeded}, nil) != nil {
		t.Error("success is an error")
	}
}
