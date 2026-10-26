package compose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRunnerDockerConfigEnv(t *testing.T) {
	if env := (&Runner{}).env(); strings.Contains(strings.Join(env, "\n"), "DOCKER_CONFIG") {
		t.Fatalf("env = %v", env)
	}
	env := (&Runner{DockerConfig: "/data/docker"}).env()
	if !slices.Contains(env, "DOCKER_CONFIG=/data/docker") {
		t.Fatalf("env = %v", env)
	}
}

func TestPrepareDockerConfig(t *testing.T) {
	if err := (&Runner{}).PrepareDockerConfig(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "docker")
	if err := (&Runner{DockerConfig: dir}).PrepareDockerConfig(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("stat = %v, %v", st, err)
	}
}

func TestPullArgsAndError(t *testing.T) {
	cli := fakeCLI(t, `echo "args: $*"; env | grep DOCKER_CONFIG`)
	var lines []string
	r := &Runner{CLI: cli, DockerConfig: "/cfg"}
	file := filepath.Join(t.TempDir(), "c.json")
	if err := r.Pull(context.Background(), file, "p", func(_, l string) { lines = append(lines, l) }); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lines, "\n")
	want := "args: compose --ansi never --project-directory " + filepath.Dir(file) + " -f " + file + " -p p pull --ignore-buildable"
	if !strings.Contains(got, want) || !strings.Contains(got, "DOCKER_CONFIG=/cfg") {
		t.Fatalf("output = %q", got)
	}
	err := (&Runner{CLI: fakeCLI(t, `echo denied >&2; exit 1`)}).Pull(context.Background(), file, "p", nil)
	var ce *Error
	if !errors.As(err, &ce) || ce.Op != "pull" || !strings.Contains(ce.Stderr, "denied") {
		t.Fatalf("err = %v", err)
	}
}
