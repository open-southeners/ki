package cli

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/controlplane"
	"github.com/open-southeners/ki-agent/internal/enroll"
	"github.com/open-southeners/ki-agent/internal/enrollment"
	"github.com/open-southeners/ki-agent/internal/enrollment/enrolltest"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/server"
	"github.com/open-southeners/ki-agent/internal/version"
)

// enrollAgent is the real server, control-plane manager and enrollment
// handlers on a temp socket, next to a fake control plane.
type enrollAgent struct {
	sock string
	dir  string
	cp   *enrolltest.ControlPlane
	mgr  *controlplane.Manager
}

func startEnrollAgent(t *testing.T) *enrollAgent {
	t.Helper()
	cp := enrolltest.New(t)
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Socket.Path = filepath.Join(dir, "ki.sock")
	cfg.ControlPlane.ReconnectMin = 10 * time.Millisecond
	cfg.ControlPlane.ReconnectMax = 50 * time.Millisecond
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.API.Listen = "127.0.0.1:0"
	cfg.API.EdgeListen = "127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := rpc.NewRouter()
	ag := agent.New()
	ag.Register(router)
	srv, err := server.New(server.Options{Config: &cfg, Router: router, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	mgr := controlplane.NewManager(log)
	h := enrollment.New(enrollment.Options{
		Client:  enroll.NewClient(enroll.Options{Sleep: enrolltest.FastSleep}),
		Manager: mgr, Config: cfg.ControlPlane, DataDir: dir, DefaultURL: cp.URL,
		Serve:  func(ctx context.Context, c rpc.Conn) { srv.ServeConn(ctx, rpc.TransportControlPlane, c) },
		Logger: log,
	})
	h.Register(router)
	ag.SetControlPlane(mgr.StatusAny)
	if err := h.Apply(); err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = mgr.Run(ctx) }()
	go func() { defer wg.Done(); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return &enrollAgent{sock: cfg.Socket.Path, dir: dir, cp: cp, mgr: mgr}
}

// syncBuffer is a bytes.Buffer safe for a command writing while a test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runningCLI is a command executing in the background.
type runningCLI struct {
	out  *syncBuffer
	done chan error
}

func startCLI(ctx context.Context, sock string, args ...string) *runningCLI {
	cmd := newRootCmd()
	r := &runningCLI{out: &syncBuffer{}, done: make(chan error, 1)}
	cmd.SetOut(r.out)
	cmd.SetErr(r.out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs(append([]string{"agent", "--socket", sock}, args...))
	go func() { r.done <- cmd.ExecuteContext(ctx) }()
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *runningCLI) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("command did not finish; output:\n%s", r.out.String())
		return nil
	}
}

func TestEnrollEndToEnd(t *testing.T) {
	a := startEnrollAgent(t)

	r := startCLI(context.Background(), a.sock, "enroll", a.cp.URL)
	waitFor(t, "the code to be printed", func() bool { return strings.Contains(r.out.String(), enrolltest.UserCode) })
	out := r.out.String()
	if !strings.Contains(out, "Open "+a.cp.URL+"/activate?code="+enrolltest.UserCode+" and confirm the code "+enrolltest.UserCode) {
		t.Errorf("output:\n%s", out)
	}
	select {
	case err := <-r.done:
		t.Fatalf("enroll returned before approval: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	a.cp.Approve()
	if err := r.wait(t); err != nil {
		t.Fatalf("enroll: %v\n%s", err, r.out.String())
	}
	if want := "Enrolled. The agent is connecting to " + a.cp.WSURL(enrolltest.AgentPath); !strings.Contains(r.out.String(), want) {
		t.Errorf("output missing %q:\n%s", want, r.out.String())
	}
	for _, secret := range []string{enrolltest.Token, enrolltest.DeviceCode} {
		if strings.Contains(r.out.String(), secret) {
			t.Errorf("output leaks a secret: %s", r.out.String())
		}
	}

	// The agent is connected without a restart, and status says where from.
	waitFor(t, "control plane connection", func() bool {
		return a.cp.Live(enrolltest.AgentPath) == 1 && a.mgr.Status().State == controlplane.StateConnected
	})
	stdout, _, err := runCLI(t, a.sock, "", "status")
	if err != nil {
		t.Fatal(err)
	}
	if want := "connected " + a.cp.WSURL(enrolltest.AgentPath) + " (enrollment)"; !strings.Contains(stdout, want) {
		t.Errorf("status missing %q:\n%s", want, stdout)
	}

	// Enrolling again needs --force.
	_, _, err = runCLI(t, a.sock, "", "enroll", a.cp.URL)
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("second enroll error = %v", err)
	}

	stdout, _, err = runCLI(t, a.sock, "", "unenroll", "--yes")
	if err != nil || !strings.Contains(stdout, "Enrollment removed") {
		t.Fatalf("unenroll: %q, %v", stdout, err)
	}
	waitFor(t, "disconnect", func() bool { return a.cp.Live(enrolltest.AgentPath) == 0 })
	if _, err := os.Stat(filepath.Join(a.dir, enrollment.CredentialsFile)); !os.IsNotExist(err) {
		t.Errorf("credentials file still there: %v", err)
	}
	stdout, _, _ = runCLI(t, a.sock, "", "status")
	if !strings.Contains(stdout, "Control plane  disabled") {
		t.Errorf("status after unenroll:\n%s", stdout)
	}
	_, _, err = runCLI(t, a.sock, "", "unenroll", "--yes")
	if err == nil || err.Error() != "not enrolled" {
		t.Errorf("second unenroll error = %v", err)
	}
}

func TestEnrollCancelStopsServerSide(t *testing.T) {
	a := startEnrollAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startCLI(ctx, a.sock, "enroll", a.cp.URL)
	waitFor(t, "polling", func() bool { return a.cp.Polls() > 0 })

	cancel() // Ctrl-C
	if err := r.wait(t); err == nil || err.Error() != "enrollment cancelled" {
		t.Fatalf("err = %v", err)
	}
	// Polling stops and a new enrollment can start.
	waitFor(t, "polling to stop", func() bool {
		n := a.cp.Polls()
		time.Sleep(50 * time.Millisecond)
		return a.cp.Polls() == n
	})
	r2 := startCLI(context.Background(), a.sock, "enroll", a.cp.URL)
	waitFor(t, "second enrollment to print", func() bool { return strings.Contains(r2.out.String(), enrolltest.UserCode) })
	a.cp.Approve()
	if err := r2.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollDenied(t *testing.T) {
	a := startEnrollAgent(t)
	a.cp.Deny()
	_, _, err := runCLI(t, a.sock, "", "enroll", a.cp.URL)
	if err == nil || !strings.Contains(err.Error(), "denied") || !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnrollWithoutURL(t *testing.T) {
	orig := version.EnrollURL
	t.Cleanup(func() { version.EnrollURL = orig })

	a := startEnrollAgent(t)
	version.EnrollURL = ""
	_, _, err := runCLI(t, a.sock, "", "enroll")
	if err == nil || !strings.Contains(err.Error(), "pass it as an argument") {
		t.Fatalf("err = %v", err)
	}
	if a.cp.Devices() != 0 {
		t.Error("agent was contacted without a URL")
	}

	// With a built-in default the CLI sends no URL and the agent uses its own
	// (the harness agent defaults to the fake control plane).
	version.EnrollURL = "https://built-in.example.test"
	r := startCLI(context.Background(), a.sock, "enroll")
	waitFor(t, "code", func() bool { return strings.Contains(r.out.String(), enrolltest.UserCode) })
	a.cp.Approve()
	if err := r.wait(t); err != nil {
		t.Fatalf("%v\n%s", err, r.out.String())
	}
}

func TestUnenrollConfirmation(t *testing.T) {
	orig := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = orig })

	tests := []struct {
		name    string
		tty     bool
		stdin   string
		wantErr string
	}{
		{"non-tty", false, "", "pass --yes"},
		{"declined", true, "n\n", "aborted"},
		{"empty", true, "", "no confirmation"},
		{"accepted", true, "y\n", "not enrolled"}, // gets as far as the agent
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := startEnrollAgent(t)
			stdinIsTerminal = func(io.Reader) bool { return tt.tty }
			_, _, err := runCLI(t, a.sock, tt.stdin, "unenroll")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
