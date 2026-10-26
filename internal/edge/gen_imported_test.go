package edge

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func routerTLS(t *testing.T, m Model, project string) any {
	t.Helper()
	b, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		HTTP struct {
			Routers map[string]struct {
				TLS any `yaml:"tls"`
			} `yaml:"routers"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for name, r := range doc.HTTP.Routers {
		if strings.HasPrefix(name, "p-"+project+"-") {
			return r.TLS
		}
	}
	t.Fatalf("no router for %s", project)
	return nil
}

func TestDynamicImportedRouting(t *testing.T) {
	m := Model{
		WebhooksPort: 8090,
		Imported:     []ImportedCert{{ID: "i-0123456789ab", Names: []string{"*.example.com"}}},
		Routes: []ProjectRoute{
			{Project: "covered", Service: "web", Domains: []string{"a.example.com", "b.example.com"}, Servers: []string{"http://172.20.0.2:80"}},
			{Project: "partial", Service: "web", Domains: []string{"a.example.com", "other.example.org"}, Servers: []string{"http://172.20.0.3:80"}},
			{Project: "deep", Service: "web", Domains: []string{"a.b.example.com"}, Servers: []string{"http://172.20.0.4:80"}},
			{Project: "apex", Service: "web", Domains: []string{"example.com"}, Servers: []string{"http://172.20.0.5:80"}},
		},
	}
	if got, _ := routerTLS(t, m, "covered").(map[string]any); got == nil || len(got) != 0 {
		t.Errorf("covered tls = %#v, want empty mapping", got)
	}
	for _, p := range []string{"partial", "deep", "apex"} {
		got, _ := routerTLS(t, m, p).(map[string]any)
		if got["certResolver"] != "le" {
			t.Errorf("%s tls = %#v, want certResolver le", p, got)
		}
	}
	// An installed certificate covers exactly its hostname.
	m.Installed = []InstalledCert{{Hostname: "example.com"}}
	if got, _ := routerTLS(t, m, "apex").(map[string]any); got == nil || len(got) != 0 {
		t.Errorf("apex with installed cert tls = %#v", got)
	}
}

func TestDynamicImportedServerHostnames(t *testing.T) {
	m := Model{
		WebhooksPort: 8090,
		Imported: []ImportedCert{
			{ID: "i-0123456789ab", Names: []string{"*.example.net", "ops.example.net"}, Server: true},
			{ID: "i-ba9876543210", Names: []string{"other.example.net"}},
		},
	}
	b, err := Dynamic(m)
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	for _, want := range []string{"Host(`ops.example.net`)", "/etc/traefik/certs/imported/i-0123456789ab.crt", "/etc/traefik/certs/imported/i-ba9876543210.key"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"Host(`other.example.net`)", "Host(`*"} {
		if strings.Contains(out, bad) {
			t.Errorf("unexpected %q:\n%s", bad, out)
		}
	}
	// A standalone domain covered by an imported wildcard uses the store.
	m.ACMEHostname = "panel.example.net"
	b, _ = Dynamic(m)
	if strings.Contains(string(b), "certResolver") {
		t.Errorf("covered ACME hostname still uses the resolver:\n%s", b)
	}
	m.Imported[0].ID = "../x"
	if _, err := Dynamic(m); err == nil {
		t.Error("invalid imported id accepted")
	}
}
