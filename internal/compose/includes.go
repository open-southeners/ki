package compose

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxIncludeDepth = 16
	maxComposeBytes = 4 << 20
)

// IncludeError reports a compose file reference that must not be handed to
// Compose: it points outside the checkout, is not a plain local path, or the
// file cannot be read as YAML.
type IncludeError struct {
	// File is the compose file containing the reference.
	File string
	// Path is the offending reference as written; empty for a file-level
	// problem such as invalid YAML.
	Path string
	// Reason says what is wrong.
	Reason string
}

func (e *IncludeError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("compose file %s: %s", e.File, e.Reason)
	}
	return fmt.Sprintf("compose file %s: %q: %s", e.File, e.Path, e.Reason)
}

// CheckIncludes scans the raw compose YAML before Compose reads it, so that
// normalizing untrusted input cannot make Compose read files outside root. It
// follows `include` (string, list, and the long form with `path`,
// `project_directory` and `env_file`) and `services.*.extends.file`
// recursively. It also validates the files Compose reads while normalizing:
// `services.*.env_file` and `services.*.label_file` (some Compose versions
// inline env_file contents despite --no-env-resolution); these may be absolute
// if they land inside root. configs/secrets `file` and build paths are not read
// at config time and stay with the policy.
//
// A reference must be a local path without variables that stays inside root.
// Relative paths are resolved the way Compose does it when run with
// `--project-directory root`: in the main compose file against root (not the
// file's own directory), in an included or extended file against that file's
// directory. An include `project_directory` is only accepted when it is the
// included file's own directory. Compose files themselves
// must not be symlinks. composeFile is absolute or relative to root. The
// error, if any, is an [*IncludeError].
func CheckIncludes(root, composeFile string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("compose: resolve root: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return fmt.Errorf("compose: resolve root: %w", err)
	}
	s := &scanner{root: realRoot, seen: map[string]bool{}}
	file := composeFile
	if !filepath.IsAbs(file) {
		file = filepath.Join(realRoot, file)
	}
	return s.scan(file, realRoot, 0)
}

type scanner struct {
	root string
	seen map[string]bool
}

// scan checks one compose file. base is the directory its include and extends
// paths, env_file and label_file paths are relative to.
func (s *scanner) scan(file, base string, depth int) error {
	if depth > maxIncludeDepth {
		return &IncludeError{File: file, Reason: "include nesting too deep"}
	}
	if st, err := os.Lstat(file); err != nil {
		return &IncludeError{File: file, Reason: fmt.Sprintf("cannot read: %v", err)}
	} else if st.Mode()&os.ModeSymlink != 0 {
		return &IncludeError{File: file, Reason: "compose files must not be symlinks"}
	}
	real, err := filepath.EvalSymlinks(file)
	if err != nil {
		return &IncludeError{File: file, Reason: fmt.Sprintf("cannot read: %v", err)}
	}
	if !within(s.root, real) {
		return &IncludeError{File: file, Reason: "resolves outside the project"}
	}
	seenKey := real + "\x00" + base
	if s.seen[seenKey] {
		return nil
	}
	s.seen[seenKey] = true

	raw, err := readLimited(real)
	if err != nil {
		return &IncludeError{File: file, Reason: err.Error()}
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return &IncludeError{File: file, Reason: fmt.Sprintf("invalid YAML: %v", err)}
	}
	top, ok := doc.(map[string]any)
	if !ok {
		if doc == nil {
			return nil
		}
		return &IncludeError{File: file, Reason: "top-level object must be a mapping"}
	}
	dir := base

	var refs []string // compose files to descend into
	check := func(p string) (string, error) { return s.resolve(file, dir, "include/extends", p, false) }

	if inc, ok := top["include"]; ok && inc != nil {
		items, ok := inc.([]any)
		if !ok {
			items = []any{inc}
		}
		for _, it := range items {
			switch v := it.(type) {
			case string:
				r, err := check(v)
				if err != nil {
					return err
				}
				refs = append(refs, r)
			case map[string]any:
				envs, err := stringList(file, "include.env_file", v["env_file"])
				if err != nil {
					return err
				}
				for _, p := range envs {
					for _, b := range []string{dir, s.root} {
						if _, err := s.resolve(file, b, "include.env_file", p, true); err != nil {
							return err
						}
					}
				}
				paths, err := stringList(file, "include.path", v["path"])
				if err != nil {
					return err
				}
				var pds []string
				if pd, ok := v["project_directory"]; ok && pd != nil {
					if pds, err = stringList(file, "include.project_directory", pd); err != nil {
						return err
					}
				}
				for _, p := range paths {
					r, err := check(p)
					if err != nil {
						return err
					}
					// Compose would resolve the included file's own references
					// against project_directory; allowing any other directory
					// would need a second base per file, so require it to be
					// the file's directory (the default).
					for _, pd := range pds {
						for _, b := range []string{dir, s.root} {
							d, err := s.resolve(file, b, "include.project_directory", pd, false)
							if err != nil {
								return err
							}
							if d != filepath.Dir(r) {
								return &IncludeError{File: file, Path: pd, Reason: "include project_directory must be the included file's directory"}
							}
						}
					}
					refs = append(refs, r)
				}
			default:
				return &IncludeError{File: file, Reason: "include entry must be a string or a mapping"}
			}
		}
	}
	if services, ok := top["services"].(map[string]any); ok {
		for name, sv := range services {
			svc, ok := sv.(map[string]any)
			if !ok {
				continue
			}
			if err := s.diskRefs(file, dir, name, svc); err != nil {
				return err
			}
			ext, ok := svc["extends"].(map[string]any)
			if !ok {
				continue // a bare string extends a service of the same file
			}
			f, present := ext["file"]
			if !present || f == nil {
				continue
			}
			p, ok := f.(string)
			if !ok {
				return &IncludeError{File: file, Reason: fmt.Sprintf("service %q: extends.file must be a string", name)}
			}
			r, err := check(p)
			if err != nil {
				return err
			}
			refs = append(refs, r)
		}
	}
	for _, r := range refs {
		if err := s.scan(r, filepath.Dir(r), depth+1); err != nil {
			return err
		}
	}
	return nil
}

// resolve validates one reference found in file and returns its absolute
// location. dir is the directory references are relative to.
// key names the compose key for error text; allowAbs permits absolute paths
// that end up inside the root (used for files read, not compose sources).
func (s *scanner) resolve(file, dir, key, p string, allowAbs bool) (string, error) {
	bad := func(reason string) error { return &IncludeError{File: file, Path: p, Reason: key + ": " + reason} }
	switch {
	case p == "":
		return "", bad("empty path")
	case strings.Contains(p, "$"):
		return "", bad("variables are not allowed in paths")
	case strings.Contains(p, "://") || strings.HasPrefix(p, "git@"):
		return "", bad("remote sources are not allowed")
	case strings.ContainsRune(p, 0):
		return "", bad("invalid path")
	case strings.HasPrefix(p, "~") || filepath.VolumeName(p) != "":
		return "", bad("absolute paths are not allowed")
	case !allowAbs && (filepath.IsAbs(p) || strings.HasPrefix(p, "/")):
		return "", bad("absolute paths are not allowed")
	}
	full := filepath.Join(dir, p)
	if filepath.IsAbs(p) {
		full = filepath.Clean(p)
	}
	if !within(s.root, full) {
		return "", bad("escapes the project")
	}
	// A symlink inside the checkout must not lead out of it either. A
	// missing target is left for Compose to report.
	if real, err := filepath.EvalSymlinks(full); err == nil {
		if !within(s.root, real) {
			return "", bad("resolves outside the project")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", bad(fmt.Sprintf("cannot resolve: %v", err))
	}
	return full, nil
}

// diskRefs validates the keys of one service that make Compose read a file
// while normalizing: env_file (string, list of strings or {path, required,
// format} objects) and label_file (string or list). Some Compose versions
// inline env_file contents even with --no-env-resolution, so the policy alone
// cannot be relied on to catch an escaping path.
func (s *scanner) diskRefs(file, dir, name string, svc map[string]any) error {
	for _, key := range []string{"env_file", "label_file"} {
		v, ok := svc[key]
		if !ok || v == nil {
			continue
		}
		what := fmt.Sprintf("service %q: %s", name, key)
		items, ok := v.([]any)
		if !ok {
			items = []any{v}
		}
		for _, it := range items {
			var p string
			switch t := it.(type) {
			case string:
				p = t
			case map[string]any:
				str, ok := t["path"].(string)
				if !ok || key != "env_file" {
					return &IncludeError{File: file, Reason: what + " entry must have a string path"}
				}
				p = str
			default:
				return &IncludeError{File: file, Reason: what + " entry must be a string or a mapping"}
			}
			if _, err := s.resolve(file, dir, what, p, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// within reports whether path is root or below it (both cleaned, absolute).
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// stringList reads a YAML scalar string or list of strings; nil gives none.
func stringList(file, what string, v any) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			str, ok := e.(string)
			if !ok {
				return nil, &IncludeError{File: file, Reason: what + " must be a string or a list of strings"}
			}
			out = append(out, str)
		}
		return out, nil
	}
	return nil, &IncludeError{File: file, Reason: what + " must be a string or a list of strings"}
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxComposeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxComposeBytes {
		return nil, fmt.Errorf("file larger than %d bytes", maxComposeBytes)
	}
	return b, nil
}
