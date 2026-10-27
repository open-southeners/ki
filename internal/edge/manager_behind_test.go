package edge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/store"
)

func TestBehindModeReconcileAndStatus(t *testing.T) {
	cfg := enabledCfg()
	cfg.Mode, cfg.BehindListen = "behind", "127.0.0.1:8480"
	r := newRig(t, cfg)
	web, det := webContainer("w1", "172.20.0.2")
	r.rt.set(web)
	r.rt.details["w1"] = det
	r.ex.list = []store.ProjectExposure{{Project: "shop", Exposure: store.Exposure{Service: "web", Port: 8080, Domains: []string{"shop.example.com"}}}}

	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.m.Close()
	waitFor(t, "running", func() bool { return r.m.Status().State == StateRunning })

	s := r.m.Status()
	if s.Mode != "behind" || s.Listen != "127.0.0.1:8480" || s.Routes != 1 {
		t.Errorf("status = %+v", s)
	}
	// No ACME certificates are requested in behind mode.
	if len(s.Certificates) != 0 {
		t.Errorf("certificates = %+v", s.Certificates)
	}
	if got := strings.Join(s.ServerHostnames, ","); got != "panel.example.com" {
		t.Errorf("server hostnames = %s", got)
	}
	static, err := os.ReadFile(filepath.Join(r.edge, "traefik.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(static), "127.0.0.1:8480") || strings.Contains(string(static), "websecure") {
		t.Errorf("static:\n%s", static)
	}
	dyn, err := os.ReadFile(filepath.Join(r.edge, "dynamic", "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(dyn), "tls") || !strings.Contains(string(dyn), "Host(`panel.example.com`)") {
		t.Errorf("dynamic:\n%s", dyn)
	}
}

func TestFrontModeStatus(t *testing.T) {
	r := newRig(t, enabledCfg())
	if s := r.m.Status(); s.Mode != "front" || s.Listen != "" {
		t.Errorf("status = %+v", s)
	}
}
