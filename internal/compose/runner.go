// Package compose runs the Docker Compose CLI for deploys.
//
// Untrusted project files are normalized with `docker compose config`, checked
// by internal/policy, labelled by [Label] and only then built and started from
// the labelled model, so what runs is exactly what was checked. Every
// invocation gets a minimal environment (PATH, HOME and DOCKER_HOST) and never
// carries secrets in argv.
package compose

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// stderrTail is how much of a failed command's stderr is kept in the error.
const stderrTail = 64 << 10

// cancelGrace is how long a cancelled command gets after SIGINT before it is
// killed.
const cancelGrace = 10 * time.Second

// Runner invokes the Docker CLI with the Compose v2 plugin.
type Runner struct {
	// CLI is the docker executable (name looked up in PATH, or a path).
	// Empty means "docker".
	CLI string
	// DockerHost is passed as DOCKER_HOST when non-empty.
	DockerHost string
	// Home is passed as HOME (so Docker reads its config from there, not
	// from the agent's own home).
	Home string
	// Logger receives debug lines; nil discards them.
	Logger *slog.Logger
}

// Error is a failed Compose invocation.
type Error struct {
	// Op is the compose subcommand: config, build or up.
	Op string
	// Err is the underlying failure (usually *exec.ExitError).
	Err error
	// Stderr is the tail (at most 64 KiB) of the command's standard error.
	Stderr string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("compose %s: %v", e.Op, e.Err)
	if s := strings.TrimSpace(e.Stderr); s != "" {
		msg += ": " + s
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// Config prints the normalized project model as JSON, with the project
// directory dir as cwd and without resolving env_file contents
// (`config --format json --no-env-resolution`). composeFile may be relative to
// dir.
func (r *Runner) Config(ctx context.Context, dir, composeFile, project string) ([]byte, error) {
	cmd := r.command(ctx, dir, "--project-directory", dir, "-f", composeFile, "-p", project,
		"config", "--format", "json", "--no-env-resolution")
	var stdout bytes.Buffer
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stdout, cmd.Stderr = &stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, &Error{Op: "config", Err: err, Stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}

// Build runs `compose build` for the labelled model file, calling out for each
// output line with stream "stdout" or "stderr". out may be called from two
// goroutines but never concurrently.
func (r *Runner) Build(ctx context.Context, file, project string, out func(stream, line string)) error {
	return r.stream(ctx, "build", file, project, out, "build")
}

// Up runs `compose up -d --remove-orphans` for the labelled model file,
// streaming output like [Runner.Build].
func (r *Runner) Up(ctx context.Context, file, project string, out func(stream, line string)) error {
	return r.stream(ctx, "up", file, project, out, "up", "-d", "--remove-orphans")
}

func (r *Runner) stream(ctx context.Context, op, file, project string, out func(stream, line string), args ...string) error {
	dir := filepath.Dir(file)
	base := []string{"--project-directory", dir, "-f", file, "-p", project}
	cmd := r.command(ctx, dir, append(base, args...)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return &Error{Op: op, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return &Error{Op: op, Err: err}
	}
	tail := &tailBuffer{max: stderrTail}
	var mu sync.Mutex
	emit := func(stream, line string) {
		if out == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		out(stream, line)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		readLines(stdout, func(l string) { emit("stdout", l) })
	}()
	go func() {
		defer wg.Done()
		readLines(stderrPipe, func(l string) {
			tail.Write([]byte(l + "\n"))
			emit("stderr", l)
		})
	}()
	wg.Wait() // pipes must be drained before Wait closes them
	if err := cmd.Wait(); err != nil {
		return &Error{Op: op, Err: err, Stderr: tail.String()}
	}
	return nil
}

// command builds a docker compose invocation with the minimal environment.
func (r *Runner) command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cli := r.CLI
	if cli == "" {
		cli = "docker"
	}
	// --ansi never keeps the output free of terminal control sequences.
	cmd := exec.CommandContext(ctx, cli, append([]string{"compose", "--ansi", "never"}, args...)...)
	cmd.Dir = dir
	cmd.Env = r.env()
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = cancelGrace
	if r.Logger != nil {
		r.Logger.Debug("running compose", "dir", dir, "args", args)
	}
	return cmd
}

// env is the whole environment of a compose process. Nothing else is
// inherited from the agent, so COMPOSE_* and other DOCKER_* variables (and any
// agent secret) can neither steer compose nor leak into interpolation.
func (r *Runner) env() []string {
	env := []string{"PATH=" + os.Getenv("PATH")}
	if r.Home != "" {
		env = append(env, "HOME="+r.Home)
	}
	if r.DockerHost != "" {
		env = append(env, "DOCKER_HOST="+r.DockerHost)
	}
	return env
}

// readLines calls fn for every line of rd (without the line ending). Lines
// over 64 KiB are split into several calls.
func readLines(rd io.Reader, fn func(string)) {
	br := bufio.NewReaderSize(rd, 64<<10)
	for {
		line, isPrefix, err := br.ReadLine()
		if len(line) > 0 || (err == nil && !isPrefix) {
			fn(string(line))
		}
		if err != nil {
			return
		}
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
