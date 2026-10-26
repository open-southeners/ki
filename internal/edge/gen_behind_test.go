package edge

import (
	"strings"
	"testing"
)

func TestStaticBehindGolden(t *testing.T) {
	b, err := Static(StaticOptions{Behind: true, BehindListen: "127.0.0.1:8480", ACMEEmail: "ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "static_behind.yaml", b)
	for _, bad := range []string{"websecure", "redirections", "certificatesResolvers", "acme", "email"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("behind static contains %q:\n%s", bad, b)
		}
	}
	// The public ports are ignored in behind mode.
	if _, err := Static(StaticOptions{Behind: true, BehindListen: "[::1]:8480"}); err != nil {
		t.Errorf("ipv6 loopback: %v", err)
	}
}

func TestStaticBehindRejects(t *testing.T) {
	for _, l := range []string{"", "8480", "0.0.0.0:8480", "10.0.0.1:8480", "localhost:8480", "127.0.0.1:0", "127.0.0.1:x"} {
		if _, err := Static(StaticOptions{Behind: true, BehindListen: l}); err == nil {
			t.Errorf("behind listen %q accepted", l)
		}
	}
}

func TestDynamicBehindGolden(t *testing.T) {
	m := sampleModel()
	m.Behind = true
	m.APIEdgePort, m.PanelEnabled = 8092, true
	b, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dynamic_behind.yaml", b)
	s := string(b)
	for _, bad := range []string{"websecure", "tls", "certResolver", "ki-hooks-ip", "ki-panel-ip", "HostRegexp", "defaultCertificate"} {
		if strings.Contains(s, bad) {
			t.Errorf("behind dynamic contains %q:\n%s", bad, s)
		}
	}
	for _, want := range []string{"ki-hooks-acme", "ki-panel-installed", "ki-rpc-acme", "- web"} {
		if !strings.Contains(s, want) {
			t.Errorf("behind dynamic missing %q", want)
		}
	}
}

func TestDynamicBehindMinimalHasNoServerRoutes(t *testing.T) {
	b, err := Dynamic(Model{WebhooksPort: 8090, Behind: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "ki-hooks") {
		t.Errorf("hooks route without a server hostname:\n%s", b)
	}
}
