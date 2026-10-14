package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// engineSockets are well-known container-engine control sockets. Any path
// whose base name is docker.sock is treated the same way.
var engineSockets = []string{
	"/var/run/docker.sock",
	"/run/docker.sock",
	"/run/containerd/containerd.sock",
	"/var/run/containerd/containerd.sock",
	"/run/podman/podman.sock",
	"/var/run/podman/podman.sock",
}

// resolvePath returns the cleaned absolute path of p with symlinks resolved on
// the longest existing prefix; the non-existent remainder is appended as is.
// A prefix that exists but cannot be resolved (dangling symlink, loop,
// permission denied) is an error, never a pass.
func resolvePath(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path %q is not absolute", p)
	}
	p = filepath.Clean(p)
	prefix, rest := p, ""
	for {
		_, err := os.Lstat(prefix)
		if err == nil {
			real, err := filepath.EvalSymlinks(prefix)
			if err != nil {
				return "", fmt.Errorf("resolve %q: %w", prefix, err)
			}
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("stat %q: %w", prefix, err)
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", fmt.Errorf("no existing prefix of %q", p)
		}
		rest = filepath.Join(filepath.Base(prefix), rest)
		prefix = parent
	}
}

// within reports whether path is root or lies below it. Both must be clean
// absolute paths; the comparison is component-wise, so /srv/app-evil is not
// within /srv/app.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// touchesEngineSocket reports whether mounting p would expose a container
// engine socket: p is one, is named docker.sock, or is a parent directory of
// one (including /).
func touchesEngineSocket(p string) bool {
	p = filepath.Clean(p)
	if filepath.Base(p) == "docker.sock" {
		return true
	}
	for _, s := range engineSockets {
		if within(p, s) {
			return true
		}
	}
	return false
}

// isRemoteContext reports whether a build context is not a local path.
func isRemoteContext(c string) bool {
	if i := strings.Index(c, "://"); i > 0 {
		return true
	}
	return strings.HasPrefix(c, "git@") || strings.HasPrefix(c, "service:")
}

// symlinkTrail returns p followed by the full path at every symlink hop met
// while walking it, without requiring any target to exist. It lets the socket
// rule see where a link points even when the check runs on a machine that has
// no such socket (a dangling link to /var/run/docker.sock still counts).
func symlinkTrail(p string) []string {
	p = filepath.Clean(p)
	trail := []string{p}
	parts := splitPath(p)
	built := "/"
	for hops := 0; len(parts) > 0; {
		next := filepath.Join(built, parts[0])
		parts = parts[1:]
		fi, err := os.Lstat(next)
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			built = next
			continue
		}
		if hops++; hops > 40 {
			break
		}
		t, err := os.Readlink(next)
		if err != nil {
			break
		}
		if !filepath.IsAbs(t) {
			t = filepath.Join(built, t)
		}
		parts = append(splitPath(filepath.Clean(t)), parts...)
		built = "/"
		trail = append(trail, filepath.Join(append([]string{"/"}, parts...)...))
	}
	return trail
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// touchesEngineSocketVia is touchesEngineSocket for p and everything its
// symlinks lead through, plus the resolved path (if any).
func touchesEngineSocketVia(p, resolved string) bool {
	if resolved != "" && touchesEngineSocket(resolved) {
		return true
	}
	for _, t := range symlinkTrail(p) {
		if touchesEngineSocket(t) {
			return true
		}
	}
	return false
}
