package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/rpc"
)

const tunnelCLIToken = "eyJhIjoiQ0xJU0VDUkVUIiwidCI6IngifQ=="

const tunnelStatusJSON = `{"enabled":true,"state":"disconnected","configured":true,"ready":false,"connections":0,
"image":"cloudflare/cloudflared:2026.10.0","last_error":"","warnings":["edge.mode is not behind"]}`

func tunnelStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "tunnel.status", tunnelStatusJSON))
		r.Handle(stub(rec, "tunnel.configure", tunnelStatusJSON))
		r.Handle(stub(rec, "tunnel.remove", `{"enabled":true,"state":"unconfigured","configured":false,"ready":false,"connections":0,"image":"x","warnings":[]}`))
	}
}

func TestTunnelStatus(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, tunnelStubs(rec))
	out, _, err := runCLI(t, sock, "", "tunnel", "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"disconnected", "Ready", "no", "cloudflare/cloudflared:2026.10.0", "Warning", "edge.mode is not behind"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	out, _, err = runCLI(t, sock, "", "tunnel", "status", "--json")
	if err != nil || !strings.Contains(out, `"state": "disconnected"`) {
		t.Errorf("json output = %q, %v", out, err)
	}
}

func TestTunnelConfigure(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, tunnelStubs(rec))

	out, errOut, err := runCLI(t, sock, " "+tunnelCLIToken+"\n", "tunnel", "configure", "--token-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Tunnel token stored.") {
		t.Errorf("output = %q", out)
	}
	if tok := rec.last(t, "tunnel.configure")["token"]; tok != tunnelCLIToken {
		t.Errorf("token param = %v", tok)
	}
	if strings.Contains(out+errOut, tunnelCLIToken) {
		t.Error("the token was printed")
	}

	f := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(f, []byte(tunnelCLIToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, sock, "", "tunnel", "configure", "--token-file", f); err != nil {
		t.Fatal(err)
	}
	if tok := rec.last(t, "tunnel.configure")["token"]; tok != tunnelCLIToken {
		t.Errorf("token param = %v", tok)
	}

	for name, args := range map[string][]string{
		"no source":    {"tunnel", "configure"},
		"both sources": {"tunnel", "configure", "--token-stdin", "--token-file", f},
		"missing file": {"tunnel", "configure", "--token-file", f + ".nope"},
	} {
		if _, _, err := runCLI(t, sock, "", args...); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	if _, _, err := runCLI(t, sock, "  \n", "tunnel", "configure", "--token-stdin"); err == nil {
		t.Error("empty stdin should fail")
	}
}

func TestTunnelRemove(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, tunnelStubs(rec))
	out, _, err := runCLI(t, sock, "", "tunnel", "remove")
	if err != nil || !strings.Contains(out, "Tunnel removed.") {
		t.Fatalf("out = %q, %v", out, err)
	}
	if len(rec.m["tunnel.remove"]) != 1 {
		t.Errorf("tunnel.remove calls = %d", len(rec.m["tunnel.remove"]))
	}
}
