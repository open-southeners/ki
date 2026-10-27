package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/version"
)

func TestVersionCmd(t *testing.T) {
	want := "ki " + version.Version + " (commit " + version.Commit + ")\n"
	for _, args := range [][]string{{"version"}, {"agent", "version"}} {
		cmd := newRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("%v: output %q, want %q", args, out.String(), want)
		}
	}
}

func TestRootHelpListsAgentAndVersion(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"agent ", "version "} {
		if !strings.Contains(out.String(), "\n  "+want) {
			t.Errorf("help missing command %q:\n%s", want, out.String())
		}
	}
}

func TestAgentTreeCommands(t *testing.T) {
	agent, _, err := newRootCmd().Find([]string{"agent"})
	if err != nil || agent.Name() != "agent" {
		t.Fatalf("agent command: %v %v", agent, err)
	}
	for _, name := range []string{"run", "status", "projects", "deploy", "deploys", "services", "containers", "logs",
		"stats", "edge", "tunnel", "aws", "users", "tokens", "panel", "audit", "enroll", "unenroll", "update", "version"} {
		if c, _, err := agent.Find([]string{name}); err != nil || c.Name() != name {
			t.Errorf("ki agent %s missing: %v", name, err)
		}
	}
}

func TestRootForDispatch(t *testing.T) {
	for argv0, want := range map[string]string{
		"ki":                      "ki",
		"/usr/local/bin/ki":       "ki",
		"ki-agent":                "ki-agent",
		"/usr/local/bin/ki-agent": "ki-agent",
	} {
		if got := rootFor(argv0).Name(); got != want {
			t.Errorf("rootFor(%q) = %q, want %q", argv0, got, want)
		}
	}
}

func TestStatusViaAlias(t *testing.T) {
	sock := startAgent(t, func(r *rpc.Router) {
		r.Handle("runtime.info", func(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
			return map[string]string{"name": "docker", "version": "27.0.1", "api_version": "1.46"}, nil
		})
		projectsStubs(&calls{})(r)
	})
	for _, argv0 := range []string{"ki-agent", "/usr/local/bin/ki-agent"} {
		cmd := rootFor(argv0)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs([]string{"--socket", sock, "status"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%s: %v", argv0, err)
		}
		if !strings.Contains(out.String(), "docker 27.0.1") {
			t.Errorf("%s: unexpected status output:\n%s", argv0, out.String())
		}
	}
	// And the same through `ki agent status`.
	out, _, err := runCLI(t, sock, "", "status")
	if err != nil || !strings.Contains(out, "docker 27.0.1") {
		t.Fatalf("ki agent status: %v\n%s", err, out)
	}
}

func TestRunRefusesOffLinux(t *testing.T) {
	old := goos
	goos = "darwin"
	t.Cleanup(func() { goos = old })
	cmd := newRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"agent", "run"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "only supported on Linux") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunAgent(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "data")
	t.Setenv("KI_DATA_DIR", data)
	t.Setenv("KI_SOCKET_PATH", filepath.Join(dir, "run", "ki.sock"))
	// Free loopback ports keep the default 8091/8092 out of the picture.
	t.Setenv("KI_API_LISTEN", freeLoopbackAddr(t))
	t.Setenv("KI_API_EDGE_LISTEN", freeLoopbackAddr(t))

	tests := []struct {
		name     string
		f        flags
		explicit bool
		wantErr  string
	}{
		{"defaults, missing config ok", flags{configPath: filepath.Join(dir, "none.yaml")}, false, ""},
		{"explicit missing config", flags{configPath: filepath.Join(dir, "none.yaml")}, true, "config file"},
		{"bad log level flag", flags{configPath: filepath.Join(dir, "none.yaml"), logLevel: "loud"}, false, "invalid log_level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var logs bytes.Buffer
			f := tt.f
			err := runAgent(ctx, &logs, &f, tt.explicit)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(logs.String(), `"msg":"ki-agent starting"`) {
					t.Fatalf("expected JSON log, got %q", logs.String())
				}
				st, err := os.Stat(data)
				if err != nil || !st.IsDir() || st.Mode().Perm() != 0o750&^umaskBits() {
					t.Fatalf("data dir not created correctly: %v %v", st, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// umaskBits returns the process umask so permission assertions hold under any umask.
func umaskBits() os.FileMode {
	old := syscallUmask(0)
	syscallUmask(old)
	return os.FileMode(old)
}


// freeLoopbackAddr returns a 127.0.0.1:port that was free a moment ago.
func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
