package compose

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLabel(t *testing.T) {
	model := `{
  "name": "app",
  "services": {
    "web": {"image": "x", "labels": {"a": "b", "ki.project": "evil"}, "ports": [{"target": 80, "published": "8080"}]},
    "db": {"image": "y", "labels": ["k=v", "bare"]},
    "bare": {"image": "z"}
  },
  "networks": {"default": {"name": "app_default", "ipam": {}}, "ext": {"name": "o", "external": true}, "nul": null},
  "volumes": {"data": {"name": "app_data"}, "shared": {"name": "s", "external": true}, "empty": null}
}`
	out, err := Label([]byte(model), "app")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Services map[string]struct {
			Labels map[string]string `json:"labels"`
		} `json:"services"`
		Networks map[string]map[string]any `json:"networks"`
		Volumes  map[string]map[string]any `json:"volumes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	managed := func(svc string, extra map[string]string) map[string]string {
		m := map[string]string{"ki.managed": "true", "ki.project": "app", "ki.service": svc}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	wantSvc := map[string]map[string]string{
		"web":  managed("web", map[string]string{"a": "b"}),
		"db":   managed("db", map[string]string{"k": "v", "bare": ""}),
		"bare": managed("bare", nil),
	}
	for n, want := range wantSvc {
		if !reflect.DeepEqual(got.Services[n].Labels, want) {
			t.Errorf("service %s labels = %v, want %v", n, got.Services[n].Labels, want)
		}
	}
	labelsOf := func(m map[string]any) map[string]any { l, _ := m["labels"].(map[string]any); return l }
	for _, n := range []string{"default", "nul"} {
		if l := labelsOf(got.Networks[n]); l["ki.managed"] != "true" || l["ki.project"] != "app" || l["ki.service"] != nil {
			t.Errorf("network %s labels = %v", n, l)
		}
	}
	for _, n := range []string{"data", "empty"} {
		if l := labelsOf(got.Volumes[n]); l["ki.managed"] != "true" {
			t.Errorf("volume %s labels = %v", n, l)
		}
	}
	if labelsOf(got.Networks["ext"]) != nil || labelsOf(got.Volumes["shared"]) != nil {
		t.Error("external resources must not be labelled")
	}
	if got.Networks["default"]["name"] != "app_default" {
		t.Errorf("other fields changed: %v", got.Networks["default"])
	}
	if !strings.Contains(string(out), `"published": "8080"`) || !strings.Contains(string(out), `"target": 80`) {
		t.Errorf("numbers/strings not preserved:\n%s", out)
	}
}

func TestLabelErrors(t *testing.T) {
	for name, in := range map[string]string{
		"not json":        `nope`,
		"trailing":        `{} {}`,
		"null":            `null`,
		"services array":  `{"services": []}`,
		"service scalar":  `{"services": {"a": 1}}`,
		"labels scalar":   `{"services": {"a": {"labels": 3}}}`,
		"label list item": `{"services": {"a": {"labels": [1]}}}`,
		"network scalar":  `{"networks": {"n": 1}}`,
	} {
		if _, err := Label([]byte(in), "p"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestCheckIncludes(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		bad   string // substring the error must contain; "" means accepted
	}{
		{"plain", map[string]string{"compose.yaml": "services: {a: {image: x}}\n"}, ""},
		{"empty file", map[string]string{"compose.yaml": ""}, ""},
		{"include string", map[string]string{"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "services: {}\n"}, ""},
		{"include list and long form", map[string]string{
			"compose.yaml": "include:\n  - sub/i.yaml\n  - path: [sub/j.yaml, sub/k.yaml]\n    env_file: sub/e.env\n    project_directory: sub\n",
			"sub/i.yaml":   "services: {}\n", "sub/j.yaml": "services: {}\n", "sub/k.yaml": "services: {}\n",
		}, ""},
		{"extends same file", map[string]string{"compose.yaml": "services:\n  a: {extends: b}\n  b: {image: x}\n"}, ""},
		{"extends file ok", map[string]string{
			"compose.yaml": "services:\n  a: {extends: {file: base.yaml, service: b}}\n", "base.yaml": "services: {b: {image: x}}\n"}, ""},
		{"include absolute", map[string]string{"compose.yaml": "include:\n  - /etc/hostname\n"}, "/etc/hostname"},
		{"include escape", map[string]string{"compose.yaml": "include: ../other.yaml\n"}, "../other.yaml"},
		{"include escape sneaky", map[string]string{"compose.yaml": "include: sub/../../x.yaml\n"}, "sub/../../x.yaml"},
		{"include long path absolute", map[string]string{"compose.yaml": "include:\n  - path: /etc/x\n"}, "/etc/x"},
		{"include path list escape", map[string]string{"compose.yaml": "include:\n  - path: [a.yaml, ../b.yaml]\n", "a.yaml": ""}, "../b.yaml"},
		{"include env_file escape", map[string]string{"compose.yaml": "include:\n  - path: a.yaml\n    env_file: /etc/passwd\n", "a.yaml": ""}, "/etc/passwd"},
		{"include project_directory escape", map[string]string{"compose.yaml": "include:\n  - path: a.yaml\n    project_directory: ..\n", "a.yaml": ""}, `".."`},
		{"include variable", map[string]string{"compose.yaml": "include: ${HOME}/x.yaml\n"}, "${HOME}"},
		{"include remote oci", map[string]string{"compose.yaml": "include: oci://example.com/x:1\n"}, "oci://"},
		{"include remote git", map[string]string{"compose.yaml": "include: git://example.com/x.git\n"}, "git://"},
		{"include home", map[string]string{"compose.yaml": "include: ~/x.yaml\n"}, "~/x.yaml"},
		{"extends absolute", map[string]string{"compose.yaml": "services:\n  a: {extends: {file: /etc/x.yaml, service: b}}\n"}, "/etc/x.yaml"},
		{"extends escape", map[string]string{"compose.yaml": "services:\n  a: {extends: {file: ../x.yaml, service: b}}\n"}, "../x.yaml"},
		{"nested include escape", map[string]string{
			"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "include: ../../x.yaml\n"}, "../../x.yaml"},
		{"nested relative to included file", map[string]string{
			"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "include: j.yaml\n", "sub/j.yaml": "services: {}\n"}, ""},
		{"nested extends escape", map[string]string{
			"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "services:\n  a: {extends: {file: ../../x.yaml, service: b}}\n"}, "../../x.yaml"},
		{"env_file relative ok", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: [.env, {path: sub/e.env, required: false, format: raw}]}\n  b: {image: x, env_file: ./b.env}\n"}, ""},
		{"label_file relative ok", map[string]string{"compose.yaml": "services:\n  a: {image: x, label_file: labels.txt}\n"}, ""},
		{"env_file string absolute", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: /etc/passwd}\n"}, "env_file"},
		{"env_file list absolute", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: [.env, /var/lib/ki-agent/secret.key]}\n"}, "/var/lib/ki-agent/secret.key"},
		{"env_file object absolute", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: [{path: /etc/passwd, required: false}]}\n"}, "/etc/passwd"},
		{"env_file escape", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: ../../secret}\n"}, "../../secret"},
		{"env_file home", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: ~/.ssh/id}\n"}, "~/.ssh/id"},
		{"env_file variable", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: \"${HOME}/.env\"}\n"}, "${HOME}"},
		{"env_file object without path", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: [{required: false}]}\n"}, "path"},
		{"env_file bad entry", map[string]string{"compose.yaml": "services:\n  a: {image: x, env_file: [1]}\n"}, "env_file"},
		{"label_file string absolute", map[string]string{"compose.yaml": "services:\n  a: {image: x, label_file: /etc/passwd}\n"}, "label_file"},
		{"label_file list escape", map[string]string{"compose.yaml": "services:\n  a: {image: x, label_file: [l.txt, ../l.txt]}\n"}, "../l.txt"},
		{"env_file escape in included file", map[string]string{
			"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "services:\n  a: {image: x, env_file: ../../x.env}\n"}, "../../x.env"},
		{"env_file absolute in included file", map[string]string{
			"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "services:\n  a: {image: x, env_file: /etc/passwd}\n"}, "/etc/passwd"},
		{"env_file relative to included file ok", map[string]string{
			"compose.yaml": "include: sub/i.yaml\n", "sub/i.yaml": "services:\n  a: {image: x, env_file: e.env}\n"}, ""},
		{"env_file escape in extended file", map[string]string{
			"compose.yaml": "services:\n  a: {extends: {file: sub/b.yaml, service: b}}\n", "sub/b.yaml": "services:\n  b: {image: x, env_file: ../../e}\n"}, "../../e"},
		{"cycle", map[string]string{"compose.yaml": "include: a.yaml\n", "a.yaml": "include: compose.yaml\n"}, ""},
		{"invalid yaml", map[string]string{"compose.yaml": "services: [\n"}, "invalid YAML"},
		{"top-level list", map[string]string{"compose.yaml": "- a\n"}, "mapping"},
		{"include bad entry", map[string]string{"compose.yaml": "include: [1]\n"}, "string or a mapping"},
		{"missing include target is reported", map[string]string{"compose.yaml": "include: nope.yaml\n"}, "cannot read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for f, c := range tt.files {
				write(t, filepath.Join(root, f), c)
			}
			err := CheckIncludes(root, "compose.yaml")
			if tt.bad == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ie *IncludeError
			if !errors.As(err, &ie) {
				t.Fatalf("err = %v, want *IncludeError", err)
			}
			if !strings.Contains(ie.Error(), tt.bad) {
				t.Fatalf("error %q does not contain %q", ie, tt.bad)
			}
		})
	}
}

func TestCheckIncludesSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "x.yaml"), "services: {}\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "compose.yaml"), "include: link/x.yaml\n")
	var ie *IncludeError
	if err := CheckIncludes(root, "compose.yaml"); !errors.As(err, &ie) {
		t.Fatalf("err = %v, want *IncludeError", err)
	}
}

func TestCheckIncludesDiskFiles(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	write(t, filepath.Join(outside, "secret.env"), "S=1\n")
	if err := os.Symlink(filepath.Join(outside, "secret.env"), filepath.Join(root, "link.env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linkdir")); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"env_file", "label_file"} {
		for _, ref := range []string{"link.env", "linkdir/secret.env"} {
			write(t, filepath.Join(root, "compose.yaml"), "services:\n  a: {image: x, "+key+": "+ref+"}\n")
			var ie *IncludeError
			if err := CheckIncludes(root, "compose.yaml"); !errors.As(err, &ie) || !strings.Contains(ie.Error(), key) {
				t.Errorf("%s %s: err = %v, want *IncludeError naming the key", key, ref, err)
			}
		}
	}

	// An absolute path that lands inside the root is fine.
	real, _ := filepath.EvalSymlinks(root)
	write(t, filepath.Join(root, "ok.env"), "A=1\n")
	write(t, filepath.Join(root, "compose.yaml"), "services:\n  a: {image: x, env_file: "+filepath.Join(real, "ok.env")+"}\n")
	if err := CheckIncludes(root, "compose.yaml"); err != nil {
		t.Fatalf("absolute env_file inside root: %v", err)
	}
}

// TestCheckIncludesBaseDirectories pins the resolution rules observed with
// the real `docker compose --project-directory <root> -f sub/x.yaml config`:
// in the main file every relative path (env_file, include, extends.file) is
// relative to the project directory, not the file's own directory; in an
// included or extended file they are relative to that file's directory.
func TestCheckIncludesBaseDirectories(t *testing.T) {
	tests := []struct {
		name  string
		file  string // compose file, relative to root
		files map[string]string
		bad   string
	}{
		{"main env_file escapes via ..", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "services:\n  a: {image: x, env_file: ../secret}\n"}, "../secret"},
		{"main env_file root-relative ok", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "services:\n  a: {image: x, env_file: [sub/e.env, root.env]}\n"}, ""},
		{"main env_file sibling-relative escapes root", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "services:\n  a: {image: x, env_file: ../../x}\n"}, "../../x"},
		{"main include root-relative ok", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "include: other/inc.yaml\n", "other/inc.yaml": "services: {}\n"}, ""},
		{"main include dotdot escapes root", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "include: ../other/inc.yaml\n", "other/inc.yaml": "services: {}\n"}, "../other/inc.yaml"},
		{"main extends root-relative ok", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "services:\n  a: {extends: {file: other/ext.yaml, service: b}}\n", "other/ext.yaml": "services: {b: {image: x}}\n"}, ""},
		{"main extends dotdot escapes root", "sub/compose.yaml", map[string]string{
			"sub/compose.yaml": "services:\n  a: {extends: {file: ../other/ext.yaml, service: b}}\n", "other/ext.yaml": "services: {b: {image: x}}\n"}, "../other/ext.yaml"},
		{"included env_file relative to included dir ok", "compose.yaml", map[string]string{
			"compose.yaml": "include: other/inc.yaml\n", "other/inc.yaml": "services:\n  i: {image: x, env_file: ../x.env}\n"}, ""},
		{"included env_file escapes", "compose.yaml", map[string]string{
			"compose.yaml": "include: other/inc.yaml\n", "other/inc.yaml": "services:\n  i: {image: x, env_file: ../../x.env}\n"}, "../../x.env"},
		{"included nested include and extends relative to included dir", "compose.yaml", map[string]string{
			"compose.yaml":      "include: other/inc.yaml\n",
			"other/inc.yaml":    "include: deep/n.yaml\nservices:\n  x: {extends: {file: deep/e.yaml, service: e}}\n",
			"other/deep/n.yaml": "services: {}\n", "other/deep/e.yaml": "services: {e: {image: x}}\n"}, ""},
		{"included nested include is not root-relative", "compose.yaml", map[string]string{
			"compose.yaml": "include: other/inc.yaml\n", "other/inc.yaml": "include: ../../n.yaml\n"}, "../../n.yaml"},
		{"project_directory equal to included dir ok", "compose.yaml", map[string]string{
			"compose.yaml": "include:\n  - path: other/inc.yaml\n    project_directory: other\n", "other/inc.yaml": "services:\n  i: {image: x, env_file: p.env}\n"}, ""},
		{"project_directory other dir rejected", "compose.yaml", map[string]string{
			"compose.yaml": "include:\n  - path: other/inc.yaml\n    project_directory: pd\n", "other/inc.yaml": "services: {}\n"}, "must be the included file's directory"},
		{"project_directory root rejected for nested file", "compose.yaml", map[string]string{
			"compose.yaml": "include:\n  - path: other/inc.yaml\n    project_directory: .\n", "other/inc.yaml": "services: {}\n"}, "must be the included file's directory"},
		{"project_directory in included file must match its child", "compose.yaml", map[string]string{
			"compose.yaml": "include: other/inc.yaml\n", "other/inc.yaml": "include:\n  - path: deep/n.yaml\n    project_directory: .\n"}, "must be the included file's directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for f, c := range tt.files {
				write(t, filepath.Join(root, f), c)
			}
			err := CheckIncludes(root, tt.file)
			if tt.bad == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var ie *IncludeError
			if !errors.As(err, &ie) || !strings.Contains(ie.Error(), tt.bad) {
				t.Fatalf("err = %v, want *IncludeError naming %q", err, tt.bad)
			}
		})
	}
}

func TestCheckIncludesRejectsSymlinkedComposeFiles(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "real.yaml"), "services: {}\n")
	if err := os.Symlink("real.yaml", filepath.Join(root, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	var ie *IncludeError
	if err := CheckIncludes(root, "compose.yaml"); !errors.As(err, &ie) || !strings.Contains(ie.Error(), "symlink") {
		t.Fatalf("main: err = %v", err)
	}

	write(t, filepath.Join(root, "main.yaml"), "include: link.yaml\n")
	if err := os.Symlink("real.yaml", filepath.Join(root, "link.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := CheckIncludes(root, "main.yaml"); !errors.As(err, &ie) || !strings.Contains(ie.Error(), "symlink") {
		t.Fatalf("included: err = %v", err)
	}

	write(t, filepath.Join(root, "ext.yaml"), "services:\n  a: {extends: {file: link.yaml, service: b}}\n")
	if err := CheckIncludes(root, "ext.yaml"); !errors.As(err, &ie) || !strings.Contains(ie.Error(), "symlink") {
		t.Fatalf("extended: err = %v", err)
	}
}

func TestCheckIncludesAbsoluteComposeFile(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "c.yaml"), "services: {}\n")
	if err := CheckIncludes(root, filepath.Join(root, "c.yaml")); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	write(t, filepath.Join(other, "c.yaml"), "services: {}\n")
	if err := CheckIncludes(root, filepath.Join(other, "c.yaml")); err == nil {
		t.Fatal("compose file outside the root must be rejected")
	}
}

func TestWriteDotenvFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	vars := map[string]string{"B": "two words", "A": "it's", "C": "", "D": "a\nb", "E": "$X # y \\"}
	if err := WriteDotenv(path, vars); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "A=\"it's\"\nB='two words'\nC=''\nD='a\nb'\nE=\"\\$X # y \\\\\"\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	// Overwriting keeps 0600 and leaves no temp files behind.
	if err := WriteDotenv(path, map[string]string{"A": "1"}); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

func TestWriteDotenvRejects(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	for _, vars := range []map[string]string{{"1A": "x"}, {"A B": "x"}, {"A=B": "x"}, {"": "x"}, {"A": "x\x00"}} {
		if err := WriteDotenv(path, vars); err == nil {
			t.Errorf("%q: want error", vars)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("nothing must be written on error")
	}
}

func TestTailBuffer(t *testing.T) {
	tb := &tailBuffer{max: 4}
	tb.Write([]byte("ab"))
	tb.Write([]byte("cdef"))
	if tb.String() != "cdef" {
		t.Fatalf("got %q", tb.String())
	}
}

// fakeCLI writes an executable shell script standing in for docker.
func fakeCLI(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunnerMinimalEnvAndArgs(t *testing.T) {
	t.Setenv("COMPOSE_PROJECT_NAME", "leak")
	t.Setenv("KI_SECRET", "leak")
	t.Setenv("DOCKER_CONTEXT", "leak")
	dir := t.TempDir()
	cli := fakeCLI(t, `echo "args: $*"; echo "pwd: $(pwd -P)"; env | sort`)
	r := &Runner{CLI: cli, DockerHost: "tcp://h:1", Home: "/home/x"}
	out, err := r.Config(context.Background(), dir, "compose.yaml", "slug")
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	real, _ := filepath.EvalSymlinks(dir)
	for _, want := range []string{
		"args: compose --ansi never --project-directory " + dir + " -f compose.yaml -p slug config --format json --no-env-resolution",
		"pwd: " + real, "DOCKER_HOST=tcp://h:1", "HOME=/home/x", "PATH=",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"leak", "KI_SECRET", "COMPOSE_", "DOCKER_CONTEXT"} {
		if strings.Contains(s, bad) {
			t.Errorf("environment leaked %q:\n%s", bad, s)
		}
	}
}

func TestRunnerOmitsEmptyHostAndHome(t *testing.T) {
	env := (&Runner{}).env()
	if len(env) != 1 || !strings.HasPrefix(env[0], "PATH=") {
		t.Fatalf("env = %v", env)
	}
}

func TestConfigErrorCarriesStderr(t *testing.T) {
	cli := fakeCLI(t, `echo "bad file" >&2; exit 3`)
	_, err := (&Runner{CLI: cli}).Config(context.Background(), t.TempDir(), "c.yaml", "p")
	var ce *Error
	if !errors.As(err, &ce) || ce.Op != "config" || !strings.Contains(ce.Stderr, "bad file") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "bad file") {
		t.Fatalf("message lacks stderr: %v", err)
	}
}

func TestBuildAndUpStreamLines(t *testing.T) {
	cli := fakeCLI(t, `echo "out1"; echo "err1" >&2; printf 'out2'; echo " args=$*"`)
	r := &Runner{CLI: cli}
	file := filepath.Join(t.TempDir(), "compose.json")
	tests := []struct {
		name string
		run  func(func(string, string)) error
		args string
	}{
		{"build", func(o func(string, string)) error { return r.Build(context.Background(), file, "p", o) }, " -p p build"},
		{"up", func(o func(string, string)) error { return r.Up(context.Background(), file, "p", o) }, " -p p up -d --no-build --remove-orphans"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var lines []string
			if err := tt.run(func(stream, line string) { lines = append(lines, stream+": "+line) }); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(lines, "\n")
			for _, want := range []string{"stdout: out1", "stderr: err1", "stdout: out2 args=", tt.args} {
				if !strings.Contains(joined, want) {
					t.Errorf("missing %q in %q", want, joined)
				}
			}
		})
	}
}

func TestStreamFailureKeepsStderrTail(t *testing.T) {
	cli := fakeCLI(t, `echo "boom" >&2; exit 1`)
	var streamed []string
	err := (&Runner{CLI: cli}).Up(context.Background(), filepath.Join(t.TempDir(), "c.json"), "p",
		func(s, l string) { streamed = append(streamed, s+":"+l) })
	var ce *Error
	if !errors.As(err, &ce) || ce.Op != "up" || !strings.Contains(ce.Stderr, "boom") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(streamed, []string{"stderr:boom"}) {
		t.Fatalf("streamed = %v", streamed)
	}
}

func TestStreamCancelSendsInterrupt(t *testing.T) {
	cli := fakeCLI(t, `trap 'echo got-int; exit 130' INT; echo ready; while true; do sleep 0.05; done`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	var mu sync.Mutex
	var seen []string
	done := make(chan error, 1)
	go func() {
		done <- (&Runner{CLI: cli}).Build(ctx, filepath.Join(t.TempDir(), "c.json"), "p", func(_, l string) {
			mu.Lock()
			seen = append(seen, l)
			mu.Unlock()
			if l == "ready" {
				close(ready)
			}
		})
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want error after cancel")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancel did not stop the process")
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(strings.Join(seen, ","), "got-int") {
		t.Fatalf("process did not receive SIGINT: %v", seen)
	}
}

func TestReadLinesSplitsLongLines(t *testing.T) {
	long := strings.Repeat("x", 100<<10)
	var got []string
	readLines(strings.NewReader("a\n\n"+long+"\nlast"), func(l string) { got = append(got, l) })
	if len(got) < 5 || got[0] != "a" || got[1] != "" || got[len(got)-1] != "last" {
		t.Fatalf("got %d lines", len(got))
	}
	total := 0
	for _, l := range got[2 : len(got)-1] {
		total += len(l)
	}
	if total != len(long) {
		t.Fatalf("long line lost bytes: %d", total)
	}
}
