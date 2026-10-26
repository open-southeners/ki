package edge

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Errorf("%s differs (run with -update):\n%s", name, got)
	}
}

func TestStaticGolden(t *testing.T) {
	b, err := Static(StaticOptions{HTTPPort: 80, HTTPSPort: 443, ACMEEmail: "ops@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "static.yaml", b)
	b, err = Static(StaticOptions{HTTPPort: 18080, HTTPSPort: 18443})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "email") || !strings.Contains(string(b), ":18443") {
		t.Errorf("unexpected static config:\n%s", b)
	}
}

func TestStaticRejects(t *testing.T) {
	for _, o := range []StaticOptions{
		{HTTPPort: 0, HTTPSPort: 443},
		{HTTPPort: 80, HTTPSPort: 70000},
		{HTTPPort: 80, HTTPSPort: 443, ACMEEmail: "a b@c"},
	} {
		if _, err := Static(o); err == nil {
			t.Errorf("Static(%+v) accepted", o)
		}
	}
}

func sampleModel() Model {
	return Model{
		WebhooksPort:   8090,
		ACMEHostname:   "panel.example.com",
		Installed:      []InstalledCert{{Hostname: "z.cp.example.net"}, {Hostname: "a.cp.example.net"}},
		SelfSignedCert: "selfsigned.crt", SelfSignedKey: "selfsigned.key",
		Routes: []ProjectRoute{
			{Project: "shop", Service: "web", Domains: []string{"shop.example.com", "www.shop.example.com"}, Servers: []string{"http://172.20.0.3:8080", "http://172.20.0.2:8080"}},
			{Project: "blog", Service: "api", Domains: []string{"blog.example.com"}, Path: "/api", Servers: []string{"http://172.21.0.2:3000"}},
			{Project: "idle", Service: "web", Domains: []string{"idle.example.com"}},
		},
	}
}

func TestDynamicGolden(t *testing.T) {
	b, err := Dynamic(sampleModel())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dynamic.yaml", b)
	// Input order must not matter.
	m := sampleModel()
	m.Routes[0], m.Routes[1] = m.Routes[1], m.Routes[0]
	m.Installed[0], m.Installed[1] = m.Installed[1], m.Installed[0]
	b2, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Error("Dynamic is not deterministic")
	}
}

func TestDynamicMinimal(t *testing.T) {
	b, err := Dynamic(Model{WebhooksPort: 8090})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dynamic_minimal.yaml", b)
}

func TestDynamicPanelGolden(t *testing.T) {
	m := sampleModel()
	m.APIEdgePort, m.PanelEnabled = 8092, true
	b, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dynamic_panel.yaml", b)
}

func TestDynamicRPCOnlyGolden(t *testing.T) {
	m := sampleModel()
	m.APIEdgePort, m.RPCPublic = 8092, true
	b, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dynamic_rpc_public.yaml", b)
	if strings.Contains(string(b), "ki-auth-") || strings.Contains(string(b), "ki-panel-") {
		t.Errorf("panel routes without the panel:\n%s", b)
	}
}

func TestDynamicNoPanelRoutes(t *testing.T) {
	m := sampleModel()
	m.APIEdgePort = 8092 // ignored unless the panel or public RPC is on
	b, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"ki-rpc-", "ki-auth-", "ki-panel", "^.+$"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("unexpected %q:\n%s", bad, b)
		}
	}
	if _, err := Dynamic(Model{WebhooksPort: 8090, PanelEnabled: true}); err == nil {
		t.Error("panel without API edge port accepted")
	}
}

func TestIPLiteralRule(t *testing.T) {
	// Mirror the two HostRegexp alternatives; Traefik hands them the
	// lower-cased host without port and without IPv6 brackets.
	v4 := regexp.MustCompile(`^[0-9]{1,3}(\.[0-9]{1,3}){3}$`)
	v6 := regexp.MustCompile(`^\[?[0-9a-f]*:[0-9a-f:.]*\]?$`)
	if !strings.Contains(ipLiteralRule, v4.String()) || !strings.Contains(ipLiteralRule, v6.String()) {
		t.Fatalf("test patterns out of sync with %s", ipLiteralRule)
	}
	match := func(h string) bool { return v4.MatchString(h) || v6.MatchString(h) }
	for _, h := range []string{"203.0.113.7", "::1", "2001:db8::1", "[2001:db8::1]", "::ffff:10.0.0.1"} {
		if !match(h) {
			t.Errorf("%q should match", h)
		}
	}
	for _, h := range []string{"shop.example.com", "example.com", "1.2.3", "1.2.3.4.5", "a.b.c.d", "", "localhost", "1.2.3.4:80"} {
		if match(h) {
			t.Errorf("%q should not match", h)
		}
	}
}

func TestDynamicRejectsUnsafe(t *testing.T) {
	route := func(domains []string, path string) Model {
		return Model{WebhooksPort: 8090, Routes: []ProjectRoute{{
			Project: "p", Service: "s", Domains: domains, Path: path, Servers: []string{"http://10.0.0.2:80"},
		}}}
	}
	bad := map[string]Model{
		"backtick domain": route([]string{"a`.example.com"}, ""),
		"upper domain":    route([]string{"A.example.com"}, ""),
		"newline domain":  route([]string{"a.example.com\n"}, ""),
		"no domains":      route(nil, ""),
		"backtick path":   route([]string{"a.example.com"}, "/a`b"),
		"control path":    route([]string{"a.example.com"}, "/a\x01"),
		"space path":      route([]string{"a.example.com"}, "/a b"),
		"relative path":   route([]string{"a.example.com"}, "a"),
		"bad port":        {WebhooksPort: 0},
		"bad backend":     {WebhooksPort: 1, Routes: []ProjectRoute{{Project: "p", Service: "s", Domains: []string{"a.example.com"}, Servers: []string{"http://evil.example.com:80"}}}},
		"bad acme host":   {WebhooksPort: 1, ACMEHostname: "x`y.example.com"},
	}
	for name, m := range bad {
		if _, err := Dynamic(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestComposeGolden(t *testing.T) {
	b, err := Compose(ComposeOptions{
		Image: DefaultImage, Dir: "/var/lib/ki-agent/edge",
		SelfSignedCert: "selfsigned.crt", SelfSignedKey: "selfsigned.key", ConfigHash: "abc123",
	})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "compose.json", b)
	if _, err := Compose(ComposeOptions{Dir: "/x"}); err == nil {
		t.Error("empty image accepted")
	}
	if _, err := Compose(ComposeOptions{Image: "i", Dir: "relative"}); err == nil {
		t.Error("relative dir accepted")
	}
}

func TestPickIP(t *testing.T) {
	ips := map[string]string{"bridge": "172.17.0.2", "shop_default": "172.20.0.3", "other": "10.0.0.1"}
	if got := pickIP(ips, "shop"); got != "172.20.0.3" {
		t.Errorf("project network not preferred: %s", got)
	}
	if got := pickIP(ips, "none"); got != "172.17.0.2" {
		t.Errorf("fallback = %s", got)
	}
	if got := pickIP(nil, "shop"); got != "" {
		t.Errorf("empty = %s", got)
	}
}
