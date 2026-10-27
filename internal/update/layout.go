package update

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	// DefaultBinDir is where the ki binary and the ki-agent alias are installed.
	DefaultBinDir = "/usr/local/bin"
	// DefaultUnitDir is where the systemd units are installed.
	DefaultUnitDir = "/etc/systemd/system"

	cliName = "ki"
)

// layoutUnits are the units whose ExecStart lines follow the binary rename.
var layoutUnits = []string{"ki-agent.service", "ki-agent-update.service"}

// LayoutOptions configures [EnsureLayout]. The zero value of each field selects
// its default. The caller must be root; EnsureLayout does not check.
type LayoutOptions struct {
	// BinDir holds the ki binary and the ki-agent alias.
	BinDir string
	// UnitDir holds ki-agent.service and ki-agent-update.service.
	UnitDir string
	// DaemonReload reloads systemd after units were rewritten; nil runs
	// `systemctl daemon-reload`.
	DaemonReload func(ctx context.Context) error
	Log          *slog.Logger
}

// LayoutSummary says what [EnsureLayout] changed.
type LayoutSummary struct {
	// Renamed is set when ki-agent was moved to ki.
	Renamed bool
	// Linked is set when ki-agent was made a symlink to ki.
	Linked bool
	// Units lists the unit files whose ExecStart was rewritten.
	Units []string
	// UnitsLeftAlone is set when a unit could not be rewritten, so it keeps the
	// ki-agent alias form.
	UnitsLeftAlone bool
}

// Changed reports whether anything was modified.
func (s LayoutSummary) Changed() bool { return s.Renamed || s.Linked || len(s.Units) > 0 }

// Lines describes the changes for the user, one per line; empty when nothing
// changed.
func (s LayoutSummary) Lines() []string {
	var out []string
	if s.Renamed {
		out = append(out, "renamed ki-agent to ki")
	}
	if s.Linked {
		out = append(out, "ki-agent is now a symlink to ki")
	}
	if len(s.Units) > 0 {
		out = append(out, "updated "+strings.Join(s.Units, ", ")+" to run `ki agent`")
	}
	return out
}

// EnsureLayout migrates an install from the 1.0.x layout (a single ki-agent
// binary) to the current one: the binary is ki and ki-agent is a symlink to it,
// and the units run `ki agent ...`. It is idempotent, never follows symlinks it
// does not expect and leaves anything unusual as it is. Failing to rewrite the
// units is not an error: the alias keeps old units working.
func EnsureLayout(ctx context.Context, opts LayoutOptions) (LayoutSummary, error) {
	if opts.BinDir == "" {
		opts.BinDir = DefaultBinDir
	}
	if opts.UnitDir == "" {
		opts.UnitDir = DefaultUnitDir
	}
	if opts.DaemonReload == nil {
		opts.DaemonReload = systemdDaemonReload
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	var sum LayoutSummary
	if err := ensureBinaries(opts.BinDir, log, &sum); err != nil {
		return sum, err
	}
	rewriteUnits(ctx, opts, log, &sum)
	return sum, nil
}

func ensureBinaries(dir string, log *slog.Logger, sum *LayoutSummary) error {
	ki := filepath.Join(dir, cliName)
	alias := filepath.Join(dir, binaryName)

	kiSt, err := lstatOpt(ki)
	if err != nil {
		return err
	}
	aliasSt, err := lstatOpt(alias)
	if err != nil {
		return err
	}

	if kiSt != nil && !kiSt.Mode().IsRegular() {
		log.Info("layout: leaving binaries as they are; ki is not a regular file", "path", ki)
		return nil
	}
	if kiSt == nil {
		if aliasSt == nil || !aliasSt.Mode().IsRegular() {
			return nil // nothing to migrate
		}
		if err := os.Rename(alias, ki); err != nil {
			return fmt.Errorf("rename %s to %s: %w", alias, ki, err)
		}
		sum.Renamed = true
		aliasSt = nil
	}

	switch {
	case aliasSt == nil, aliasSt.Mode().IsRegular():
	case aliasSt.Mode()&fs.ModeSymlink != 0:
		if dest, err := os.Readlink(alias); err != nil || dest != cliName {
			log.Info("layout: leaving ki-agent as it is; it is a symlink elsewhere", "path", alias, "target", dest)
		}
		return nil
	default:
		log.Info("layout: leaving ki-agent as it is; unexpected file type", "path", alias)
		return nil
	}
	if err := replaceWithSymlink(alias, cliName); err != nil {
		return fmt.Errorf("link %s to %s: %w", alias, cliName, err)
	}
	sum.Linked = true
	return nil
}

// replaceWithSymlink atomically makes link a symlink to target.
func replaceWithSymlink(link, target string) error {
	tmp := link + ".new"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// lstatOpt is Lstat that reports a missing path as nil, nil.
func lstatOpt(p string) (fs.FileInfo, error) {
	st, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return st, err
}

func rewriteUnits(ctx context.Context, opts LayoutOptions, log *slog.Logger, sum *LayoutSummary) {
	oldPrefix := "ExecStart=" + filepath.Join(opts.BinDir, binaryName) + " "
	newPrefix := "ExecStart=" + filepath.Join(opts.BinDir, cliName) + " agent "
	for _, name := range layoutUnits {
		p := filepath.Join(opts.UnitDir, name)
		changed, err := rewriteUnit(p, oldPrefix, newPrefix)
		switch {
		case err == nil && changed:
			sum.Units = append(sum.Units, name)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			sum.UnitsLeftAlone = true
			log.Info("layout: units keep the ki-agent alias form", "unit", p, "reason", err.Error())
		}
	}
	if len(sum.Units) == 0 {
		return
	}
	if err := opts.DaemonReload(ctx); err != nil {
		log.Warn("layout: systemctl daemon-reload failed", "err", err)
	}
}

// rewriteUnit replaces oldPrefix at the start of lines in the unit at p with
// newPrefix, through a temp file and rename that keep the mode. It reports
// whether the file changed.
func rewriteUnit(p, oldPrefix, newPrefix string) (bool, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return false, err
	}
	if !st.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(b), "\n")
	changed := false
	for i, l := range lines {
		if strings.HasPrefix(l, oldPrefix) {
			lines[i] = newPrefix + strings.TrimPrefix(l, oldPrefix)
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	tmp := p + ".new"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return false, err
	}
	fail := func(err error) (bool, error) {
		f.Close()
		_ = os.Remove(tmp)
		return false, err
	}
	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		return fail(err)
	}
	if err := f.Chmod(st.Mode().Perm()); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

func systemdDaemonReload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, serviceCmdTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
