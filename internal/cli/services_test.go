package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/open-southeners/ki/internal/rpc"
)

const servicesJSON = `{"services":[
{"name":"web","image":"nginx","state":"partial","exposure":{"service":"web","port":8080,"domains":["a.example.com","b.example.com"],"source":"project"},
 "containers":[{"id":"1","state":"running","health":"healthy"},{"id":"2","state":"exited","health":""}]},
{"name":"db","state":"missing","containers":[]}]}`

const serviceJSON = `{"name":"web","state":"running","containers":[{"id":"1","state":"running","health":"unhealthy"}]}`

func servicesStubs(rec *calls) func(*rpc.Router) {
	return func(r *rpc.Router) {
		r.Handle(stub(rec, "projects.services", servicesJSON))
		r.Handle(stub(rec, "projects.services.start", serviceJSON))
		r.Handle(stub(rec, "projects.services.stop", serviceJSON))
		r.Handle(stub(rec, "projects.services.restart", serviceJSON))
	}
}

func TestServicesList(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, servicesStubs(rec))

	out, _, err := runCLI(t, sock, "", "services", "list", "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"SERVICE", "CONTAINERS", "HEALTH", "PORT", "DOMAINS", "web", "partial", "1/2", "healthy", "8080", "a.example.com,b.example.com", "db", "missing", "0/0"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if rec.last(t, "projects.services")["slug"] != "shop" {
		t.Error("slug not sent")
	}

	out, _, err = runCLI(t, sock, "", "services", "list", "shop", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Services []json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Services) != 2 {
		t.Fatalf("json output %q: %v", out, err)
	}
	if _, _, err := runCLI(t, sock, "", "services", "list"); err == nil {
		t.Error("missing slug should fail")
	}
}

func TestServicesActions(t *testing.T) {
	rec := &calls{}
	sock := startAgent(t, servicesStubs(rec))
	for verb, past := range map[string]string{"start": "started", "stop": "stopped", "restart": "restarted"} {
		out, _, err := runCLI(t, sock, "", "services", verb, "shop", "web")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "Service web of project shop "+past+".") || !strings.Contains(out, "unhealthy") {
			t.Errorf("%s output:\n%s", verb, out)
		}
		p := rec.last(t, "projects.services."+verb)
		if p["slug"] != "shop" || p["service"] != "web" {
			t.Errorf("%s params = %v", verb, p)
		}
	}
	out, _, err := runCLI(t, sock, "", "services", "stop", "shop", "web", "--json")
	if err != nil || !strings.Contains(out, `"state": "running"`) {
		t.Fatalf("json = %q, %v", out, err)
	}
	if _, _, err := runCLI(t, sock, "", "services", "stop", "shop"); err == nil {
		t.Error("missing service should fail")
	}
}
