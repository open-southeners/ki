package update

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type layoutEnv struct {
	bin, units string
	reloads    int
	opts       LayoutOptions
}

func newLayoutEnv(t *testing.T) *layoutEnv {
	t.Helper()
	e := &layoutEnv{bin: t.TempDir(), units: t.TempDir()}
	e.opts = LayoutOptions{
		BinDir: e.bin, UnitDir: e.units,
		DaemonReload: func(context.Context) error { e.reloads++; return nil },
	}
	return e
}

func (e *layoutEnv) unitText(execPath, verb string) string {
	return "[Service]\nUser=ki-agent\nExecStart=" + filepath.Join(e.bin, execPath) + " " + verb + "\n"
}

func (e *layoutEnv) legacyUnits(t *testing.T) {
	t.Helper()
	mustWrite(t, filepath.Join(e.units, "ki-agent.service"), e.unitText("ki-agent", "run --config /etc/x"), 0o644)
	mustWrite(t, filepath.Join(e.units, "ki-agent-update.service"), e.unitText("ki-agent", "update --from-request"), 0o640)
}

func (e *layoutEnv) ensure(t *testing.T) LayoutSummary {
	t.Helper()
	sum, err := EnsureLayout(context.Background(), e.opts)
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

func assertAlias(t *testing.T, bin string) {
	t.Helper()
	st, err := os.Lstat(filepath.Join(bin, "ki-agent"))
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("ki-agent is not a symlink: %v %v", st, err)
	}
	if dest, _ := os.Readlink(filepath.Join(bin, "ki-agent")); dest != "ki" {
		t.Errorf("ki-agent -> %q, want ki", dest)
	}
}

func TestEnsureLayoutMigratesFresh10xInstall(t *testing.T) {
	e := newLayoutEnv(t)
	mustWrite(t, filepath.Join(e.bin, "ki-agent"), "binary", 0o755)
	e.legacyUnits(t)

	sum := e.ensure(t)
	if !sum.Renamed || !sum.Linked || len(sum.Units) != 2 || sum.UnitsLeftAlone || !sum.Changed() {
		t.Errorf("summary = %+v", sum)
	}
	if len(sum.Lines()) != 3 {
		t.Errorf("lines = %v", sum.Lines())
	}
	if got := mustRead(t, filepath.Join(e.bin, "ki")); got != "binary" {
		t.Errorf("ki = %q", got)
	}
	assertAlias(t, e.bin)
	if got := mustRead(t, filepath.Join(e.units, "ki-agent.service")); got != e.unitText("ki", "agent run --config /etc/x") {
		t.Errorf("ki-agent.service = %q", got)
	}
	if got := mustRead(t, filepath.Join(e.units, "ki-agent-update.service")); got != e.unitText("ki", "agent update --from-request") {
		t.Errorf("ki-agent-update.service = %q", got)
	}
	if st, _ := os.Stat(filepath.Join(e.units, "ki-agent-update.service")); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if e.reloads != 1 {
		t.Errorf("daemon-reload ran %d times, want 1", e.reloads)
	}
}

func TestEnsureLayoutIsIdempotent(t *testing.T) {
	e := newLayoutEnv(t)
	mustWrite(t, filepath.Join(e.bin, "ki-agent"), "binary", 0o755)
	e.legacyUnits(t)
	e.ensure(t)
	e.reloads = 0

	sum := e.ensure(t)
	if sum.Changed() || sum.UnitsLeftAlone || len(sum.Lines()) != 0 {
		t.Errorf("second run changed things: %+v", sum)
	}
	if e.reloads != 0 {
		t.Errorf("daemon-reload ran on a no-op")
	}
	assertAlias(t, e.bin)
}

func TestEnsureLayoutReplacesPlainAliasNextToKi(t *testing.T) {
	e := newLayoutEnv(t)
	mustWrite(t, filepath.Join(e.bin, "ki"), "new", 0o755)
	mustWrite(t, filepath.Join(e.bin, "ki-agent"), "old", 0o755)

	sum := e.ensure(t)
	if sum.Renamed || !sum.Linked {
		t.Errorf("summary = %+v", sum)
	}
	assertAlias(t, e.bin)
	if got := mustRead(t, filepath.Join(e.bin, "ki")); got != "new" {
		t.Errorf("ki = %q", got)
	}
}

func TestEnsureLayoutAddsMissingAlias(t *testing.T) {
	e := newLayoutEnv(t)
	mustWrite(t, filepath.Join(e.bin, "ki"), "new", 0o755)
	sum := e.ensure(t)
	if sum.Renamed || !sum.Linked {
		t.Errorf("summary = %+v", sum)
	}
	assertAlias(t, e.bin)
}

func TestEnsureLayoutEmptyDirIsNoOp(t *testing.T) {
	e := newLayoutEnv(t)
	if sum := e.ensure(t); sum.Changed() {
		t.Errorf("summary = %+v", sum)
	}
	if _, err := os.Lstat(filepath.Join(e.bin, "ki-agent")); err == nil {
		t.Error("created ki-agent from nothing")
	}
}

func TestEnsureLayoutLeavesUnexpectedSymlinksAlone(t *testing.T) {
	t.Run("ki is a symlink elsewhere", func(t *testing.T) {
		e := newLayoutEnv(t)
		other := filepath.Join(t.TempDir(), "other")
		mustWrite(t, other, "x", 0o755)
		if err := os.Symlink(other, filepath.Join(e.bin, "ki")); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(e.bin, "ki-agent"), "plain", 0o755)
		if sum := e.ensure(t); sum.Renamed || sum.Linked {
			t.Errorf("summary = %+v", sum)
		}
		if got := mustRead(t, filepath.Join(e.bin, "ki-agent")); got != "plain" {
			t.Errorf("ki-agent = %q", got)
		}
		if dest, _ := os.Readlink(filepath.Join(e.bin, "ki")); dest != other {
			t.Errorf("ki -> %q", dest)
		}
	})
	t.Run("ki-agent is a symlink elsewhere", func(t *testing.T) {
		e := newLayoutEnv(t)
		mustWrite(t, filepath.Join(e.bin, "ki"), "new", 0o755)
		if err := os.Symlink("/somewhere/else", filepath.Join(e.bin, "ki-agent")); err != nil {
			t.Fatal(err)
		}
		if sum := e.ensure(t); sum.Changed() {
			t.Errorf("summary = %+v", sum)
		}
		if dest, _ := os.Readlink(filepath.Join(e.bin, "ki-agent")); dest != "/somewhere/else" {
			t.Errorf("ki-agent -> %q", dest)
		}
	})
}

func TestEnsureLayoutLeavesUnrelatedExecStart(t *testing.T) {
	e := newLayoutEnv(t)
	text := "[Service]\nExecStart=" + e.bin + "/ki-agentx run\nExecStart=/opt/ki-agent run\n"
	mustWrite(t, filepath.Join(e.units, "ki-agent.service"), text, 0o644)
	if sum := e.ensure(t); len(sum.Units) != 0 {
		t.Errorf("summary = %+v", sum)
	}
	if got := mustRead(t, filepath.Join(e.units, "ki-agent.service")); got != text {
		t.Errorf("unit changed: %q", got)
	}
	if e.reloads != 0 {
		t.Error("daemon-reload ran")
	}
}

func TestEnsureLayoutReadOnlyUnitDirIsNotAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	e := newLayoutEnv(t)
	mustWrite(t, filepath.Join(e.bin, "ki-agent"), "binary", 0o755)
	e.legacyUnits(t)
	if err := os.Chmod(e.units, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.units, 0o755) })

	sum := e.ensure(t)
	if !sum.Renamed || !sum.Linked || len(sum.Units) != 0 || !sum.UnitsLeftAlone {
		t.Errorf("summary = %+v", sum)
	}
	if got := mustRead(t, filepath.Join(e.units, "ki-agent.service")); !strings.Contains(got, filepath.Join(e.bin, "ki-agent")+" run") {
		t.Errorf("unit was touched: %q", got)
	}
	if e.reloads != 0 {
		t.Error("daemon-reload ran")
	}
}
