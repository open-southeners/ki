package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/version"
)

func TestVersionCmd(t *testing.T) {
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if want := "ki-agent " + version.Version; !strings.Contains(out.String(), want) {
		t.Fatalf("output %q missing %q", out.String(), want)
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

func syscallUmask(m int) int { return syscall.Umask(m) }

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
