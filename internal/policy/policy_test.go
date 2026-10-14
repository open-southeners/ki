package policy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// loadFixture reads testdata/<name>.json, substitutes the @ROOT@ placeholder
// with a fresh temp dir and returns the parsed model and that root.
func loadFixture(t *testing.T, name string) (*Project, string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseCompose([]byte(strings.ReplaceAll(string(b), "@ROOT@", root)))
	if err != nil {
		t.Fatalf("ParseCompose(%s): %v", name, err)
	}
	return p, root
}

func rulesOf(vs []Violation) []string {
	var r []string
	for _, v := range vs {
		r = append(r, v.Rule)
	}
	return r
}

func TestCleanFixturePasses(t *testing.T) {
	p, root := loadFixture(t, "clean")
	if got := len(p.Services); got != 2 {
		t.Fatalf("services = %d, want 2", got)
	}
	if vs := Check(root, p, Options{}); len(vs) != 0 {
		t.Fatalf("clean fixture violations: %+v", vs)
	}
}

func TestRuleFixtures(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	covered := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if name == "clean" {
			continue
		}
		rule, _, _ := strings.Cut(name, "__") // <rule>[__<variant>]
		covered[rule] = true
		t.Run(name, func(t *testing.T) {
			p, root := loadFixture(t, name)
			vs := Check(root, p, Options{})
			if len(vs) == 0 {
				t.Fatal("no violations")
			}
			for _, v := range vs {
				if v.Rule != rule {
					t.Errorf("unexpected violation %+v, want only rule %q", v, rule)
				}
			}
		})
	}
	for _, rule := range Rules {
		if rule != RuleProjectRoot && rule != RuleUnparseable && !covered[rule] {
			t.Errorf("rule %q has no fixture", rule)
		}
	}
}

func TestDockerSocketFixtureAlsoInsideRoot(t *testing.T) {
	// The socket rule fires even though ./run/docker.sock is inside the root.
	p, root := loadFixture(t, "docker_socket")
	vs := Check(root, p, Options{})
	if len(vs) != 1 || vs[0].Service != "a" {
		t.Fatalf("got %+v", vs)
	}
}

func TestParseShapes(t *testing.T) {
	p, _ := loadFixture(t, "clean")
	web := p.Services["web"]
	if web.Build == nil || !strings.HasSuffix(web.Build.Context, "/web") {
		t.Errorf("build = %+v", web.Build)
	}
	if len(web.EnvFiles) != 1 || !strings.HasSuffix(web.EnvFiles[0], "/app.env") {
		t.Errorf("env files = %v", web.EnvFiles)
	}
	if web.Sysctls["net.core.somaxconn"] != "1024" {
		t.Errorf("sysctls = %v", web.Sysctls)
	}
	if len(web.Volumes) != 4 || web.Volumes[0].Type != "bind" || web.Volumes[2].Type != "volume" {
		t.Errorf("volumes = %+v", web.Volumes)
	}
	if !web.Volumes[1].ReadOnly {
		t.Errorf("read_only lost: %+v", web.Volumes[1])
	}
	if p.Configs["nginx_conf"].File == "" || p.Secrets["db_password"].File == "" {
		t.Errorf("config/secret files lost")
	}
	if p.Volumes["dbdata"] == nil || p.Volumes["cache"].Driver != "local" {
		t.Errorf("volumes = %+v", p.Volumes)
	}
	if p.Services["db"].NetworkMode != "service:web" {
		t.Errorf("db network_mode = %q", p.Services["db"].NetworkMode)
	}
}

func TestParseVariantShapes(t *testing.T) {
	in := `{"services":{"a":{
		"build":"/x/ctx",
		"env_file":"/x/e.env",
		"sysctls":["net.a=1","kernel.b=2"],
		"devices":["/dev/sda:/dev/sda"],
		"volumes":["/h:/c:ro","named:/n","/anon","./rel:/r"],
		"command":null,"privileged":null}}}`
	p, err := ParseCompose([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	a := p.Services["a"]
	if a.Build.Context != "/x/ctx" || a.EnvFiles[0] != "/x/e.env" || a.Sysctls["kernel.b"] != "2" || len(a.Devices) != 1 {
		t.Errorf("a = %+v", a)
	}
	want := []Mount{
		{Type: "bind", Source: "/h", Target: "/c", ReadOnly: true},
		{Type: "volume", Source: "named", Target: "/n"},
		{Type: "volume", Target: "/anon"},
		{Type: "bind", Source: "./rel", Target: "/r"},
	}
	if !reflect.DeepEqual(a.Volumes, want) {
		t.Errorf("volumes = %+v", a.Volumes)
	}
}

func TestParseRejectsBadTypes(t *testing.T) {
	bad := map[string]string{
		"not json":          `nope`,
		"trailing":          `{} {}`,
		"services array":    `{"services":[]}`,
		"service string":    `{"services":{"a":"x"}}`,
		"privileged string": `{"services":{"a":{"privileged":"true"}}}`,
		"network_mode num":  `{"services":{"a":{"network_mode":5}}}`,
		"cap_add string":    `{"services":{"a":{"cap_add":"ALL"}}}`,
		"volumes object":    `{"services":{"a":{"volumes":{}}}}`,
		"volume number":     `{"services":{"a":{"volumes":[1]}}}`,
		"env_file number":   `{"services":{"a":{"env_file":3}}}`,
		"env_file obj":      `{"services":{"a":{"env_file":[{"x":1}]}}}`,
		"build number":      `{"services":{"a":{"build":1}}}`,
		"gpus number":       `{"services":{"a":{"gpus":3}}}`,
		"sysctl obj value":  `{"services":{"a":{"sysctls":{"k":{}}}}}`,
		"top volume string": `{"volumes":{"v":"x"}}`,
		"driver_opts list":  `{"volumes":{"v":{"driver_opts":[]}}}`,
		"config file num":   `{"configs":{"c":{"file":1}}}`,
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCompose([]byte(in)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// check runs Check against a one-service project built in code.
func check(t *testing.T, s Service, opts Options) []Violation {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	s.Name = "a"
	return Check(root, &Project{Services: map[string]*Service{"a": &s, "b": {Name: "b"}}}, opts)
}

func TestServiceRuleVariants(t *testing.T) {
	tests := []struct {
		name string
		s    Service
		want []string
	}{
		{"empty", Service{}, nil},
		{"net host", Service{NetworkMode: "host"}, []string{RuleNetworkMode}},
		{"net HOST", Service{NetworkMode: "HOST"}, []string{RuleNetworkMode}},
		{"net container", Service{NetworkMode: "container:x"}, []string{RuleNetworkMode}},
		{"net ns", Service{NetworkMode: "ns:/proc/1/ns/net"}, []string{RuleNetworkMode}},
		{"net service ok", Service{NetworkMode: "service:b"}, nil},
		{"net service unknown", Service{NetworkMode: "service:zzz"}, []string{RuleNetworkMode}},
		{"net none", Service{NetworkMode: "none"}, nil},
		{"net bridge", Service{NetworkMode: "bridge"}, nil},
		{"net custom", Service{NetworkMode: "mynet"}, nil},
		{"pid host", Service{Pid: "host"}, []string{RulePid}},
		{"pid container", Service{Pid: "container:x"}, []string{RulePid}},
		{"pid service ok", Service{Pid: "service:b"}, nil},
		{"ipc host", Service{Ipc: "host"}, []string{RuleIpc}},
		{"ipc container", Service{Ipc: "container:x"}, []string{RuleIpc}},
		{"ipc shareable", Service{Ipc: "shareable"}, nil},
		{"uts host", Service{Uts: "host"}, []string{RuleUts}},
		{"userns host", Service{UsernsMode: "host"}, []string{RuleUsernsMode}},
		{"cgroup host", Service{Cgroup: "host"}, []string{RuleCgroup}},
		{"cgroup private", Service{Cgroup: "private"}, nil},
		{"cap default ok", Service{CapAdd: []string{"NET_BIND_SERVICE"}}, nil},
		{"cap prefix ok", Service{CapAdd: []string{"cap_net_bind_service"}}, nil},
		{"cap sys_admin", Service{CapAdd: []string{"SYS_ADMIN"}}, []string{RuleCapAdd}},
		{"cap all", Service{CapAdd: []string{"all"}}, []string{RuleCapAdd}},
		{"gpus", Service{Gpus: 1}, []string{RuleDevices}},
		{"deploy devices", Service{DeployDevices: 1}, []string{RuleDevices}},
		{"build ssh", Service{Build: &Build{Context: "https://x/y", SSH: []string{"default"}}}, []string{RuleBuildSSH}},
		{"dockerfile remote ctx", Service{Build: &Build{Context: "https://x/y", Dockerfile: "../x"}}, nil},
		{"devices", Service{Devices: []string{"/dev/x"}}, []string{RuleDevices}},
		{"dev cgroup rules", Service{DeviceCgroupRules: []string{"a *:* rwm"}}, []string{RuleDeviceCgroupRules}},
		{"cgroup_parent", Service{CgroupParent: "x"}, []string{RuleCgroupParent}},
		{"volumes_from", Service{VolumesFrom: []string{"b"}}, []string{RuleVolumesFrom}},
		{"sysctl net ok", Service{Sysctls: map[string]string{"net.ipv4.ip_forward": "1"}}, nil},
		{"sysctl kernel", Service{Sysctls: map[string]string{"kernel.core_pattern": "|x"}}, []string{RuleSysctls}},
		{"mount type cluster", Service{Volumes: []Mount{{Type: "cluster", Source: "x", Target: "/x"}}}, []string{RuleUnparseable}},
		{"mount type empty", Service{Volumes: []Mount{{Source: "x", Target: "/x"}}}, []string{RuleUnparseable}},
		{"bind no source", Service{Volumes: []Mount{{Type: "bind", Target: "/x"}}}, []string{RuleUnparseable}},
		{"bind relative", Service{Volumes: []Mount{{Type: "bind", Source: "./x", Target: "/x"}}}, []string{RuleBindMount}},
		{"named volume", Service{Volumes: []Mount{{Type: "volume", Source: "v", Target: "/x"}}}, nil},
		{"volume path source", Service{Volumes: []Mount{{Type: "volume", Source: "/etc", Target: "/x"}}}, []string{RuleBindMount}},
		{"tmpfs", Service{Volumes: []Mount{{Type: "tmpfs", Target: "/x"}}}, nil},
		{"build remote", Service{Build: &Build{Context: "https://github.com/x/y.git", AdditionalContexts: map[string]string{"a": "docker-image://alpine", "b": "service:b"}}}, nil},
		{"build relative", Service{Build: &Build{Context: "."}}, []string{RuleBuildContext}},
		{"build additional outside", Service{Build: &Build{Context: "https://x/y", AdditionalContexts: map[string]string{"e": "/etc"}}}, []string{RuleBuildContext}},
		{"env_file relative", Service{EnvFiles: []string{"a.env"}}, []string{RuleEnvFile}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rulesOf(check(t, tc.s, Options{}))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rules = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSecurityOpt(t *testing.T) {
	bad := []string{"seccomp=unconfined", "seccomp:unconfined", "SECCOMP=Unconfined", "apparmor=unconfined",
		"label=disable", "label:disable", "label=type:spc_t", "systempaths=unconfined",
		"no-new-privileges=false", "no-new-privileges:false", " seccomp = unconfined "}
	for _, o := range bad {
		if got := rulesOf(check(t, Service{SecurityOpt: []string{o}}, Options{})); !reflect.DeepEqual(got, []string{RuleSecurityOpt}) {
			t.Errorf("%q: rules = %v", o, got)
		}
	}
	ok := []string{"no-new-privileges=true", "no-new-privileges", "seccomp=/etc/profile.json", "apparmor=docker-default", "label=user:u"}
	for _, o := range ok {
		if got := check(t, Service{SecurityOpt: []string{o}}, Options{}); len(got) != 0 {
			t.Errorf("%q: unexpected %+v", o, got)
		}
	}
}

func TestCapabilityOptions(t *testing.T) {
	s := Service{CapAdd: []string{"CHOWN", "NET_BIND_SERVICE"}}
	if got := rulesOf(check(t, s, Options{})); !reflect.DeepEqual(got, []string{RuleCapAdd}) {
		t.Errorf("default: %v", got)
	}
	if got := check(t, s, Options{AllowedCapabilities: []string{"CAP_CHOWN", "net_bind_service"}}); len(got) != 0 {
		t.Errorf("custom allowlist: %+v", got)
	}
	if got := check(t, Service{CapAdd: []string{"NET_BIND_SERVICE"}}, Options{AllowedCapabilities: []string{}}); len(got) != 1 {
		t.Errorf("empty allowlist should allow nothing: %+v", got)
	}
	if got := check(t, Service{CapAdd: []string{"ALL"}}, Options{AllowedCapabilities: []string{"ALL"}}); len(got) != 1 {
		t.Errorf("ALL must never be allowed: %+v", got)
	}
}

func TestPathContainment(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root := filepath.Join(base, "app")
	evil := filepath.Join(base, "app-evil")
	for _, d := range []string{root, evil, filepath.Join(root, "data")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Symlinks inside the project pointing out of it.
	if err := os.Symlink("/", filepath.Join(root, "rootlink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(evil, filepath.Join(root, "evillink")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "data"), filepath.Join(root, "okay")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing-target", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	// The project root itself reached through a symlink.
	rootAlias := filepath.Join(base, "alias")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, source string
		bad          bool
	}{
		{"root itself", root, false},
		{"child", filepath.Join(root, "data"), false},
		{"not yet existing child", filepath.Join(root, "new", "deep", "dir"), false},
		{"dotdot inside", filepath.Join(root, "data", "..", "data"), false},
		{"symlink inside", filepath.Join(root, "okay"), false},
		{"via root alias", filepath.Join(rootAlias, "data"), false},
		{"parent", base, true},
		{"sibling with shared prefix", evil, true},
		{"file in shared-prefix sibling", filepath.Join(evil, "x"), true},
		{"dotdot escape", filepath.Join(root, "..", "app-evil"), true},
		{"dotdot escape to missing", filepath.Join(root, "data", "..", "..", "nothing"), true},
		{"symlink to /", filepath.Join(root, "rootlink"), true},
		{"below symlink to /", filepath.Join(root, "rootlink", "etc"), true},
		{"below symlink to sibling", filepath.Join(root, "evillink", "x", "y"), true},
		{"dangling symlink", filepath.Join(root, "dangling"), true},
		{"below dangling symlink", filepath.Join(root, "dangling", "x"), true},
		{"filesystem root", "/", true},
		{"etc", "/etc", true},
	}
	for _, tc := range tests {
		for _, r := range []string{root, rootAlias} {
			t.Run(tc.name, func(t *testing.T) {
				p := &Project{Services: map[string]*Service{"a": {Name: "a", Volumes: []Mount{{Type: "bind", Source: tc.source, Target: "/x"}}, EnvFiles: []string{tc.source}}}}
				vs := Check(r, p, Options{})
				if tc.bad != (len(vs) > 0) {
					t.Fatalf("root %s source %s: violations = %+v, want bad=%v", r, tc.source, vs, tc.bad)
				}
				if tc.bad {
					rs := rulesOf(vs)
					if !contains(rs, RuleBindMount) || !contains(rs, RuleEnvFile) {
						t.Fatalf("rules = %v", rs)
					}
				}
			})
		}
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestSymlinkToSocketInsideRoot(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	link := filepath.Join(root, "sock")
	if err := os.Symlink("/var/run/docker.sock", link); err != nil {
		t.Fatal(err)
	}
	p := &Project{Services: map[string]*Service{"a": {Name: "a", Volumes: []Mount{{Type: "bind", Source: link, Target: "/s"}}}}}
	rs := rulesOf(Check(root, p, Options{}))
	if !contains(rs, RuleDockerSocket) || !contains(rs, RuleBindMount) {
		t.Fatalf("rules = %v", rs)
	}
}

func TestSymlinkToMissingSocketInsideRoot(t *testing.T) {
	// A link to a socket path must trip docker_socket even when the target
	// does not exist on the machine running the check (e.g. a container).
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.Symlink("/nonexistent-dir/docker.sock", filepath.Join(root, "l1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run/podman/podman.sock", filepath.Join(root, "l2")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/var/run", filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"l1", "l2", "dir", "dir/docker.sock"} {
		p := &Project{Services: map[string]*Service{"a": {Name: "a", Volumes: []Mount{{Type: "bind", Source: filepath.Join(root, src), Target: "/s"}}}}}
		if rs := rulesOf(Check(root, p, Options{})); !contains(rs, RuleDockerSocket) {
			t.Errorf("%s: rules = %v", src, rs)
		}
	}
}

func TestDockerSocketRule(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	tests := []struct {
		source string
		want   bool
	}{
		{"/var/run/docker.sock", true},
		{"/run/docker.sock", true},
		{"/run/containerd/containerd.sock", true},
		{"/run/podman/podman.sock", true},
		{"/some/where/docker.sock", true},
		{filepath.Join(root, "docker.sock"), true},
		{"/run", true},
		{"/var/run", true},
		{"/var", true},
		{"/run/containerd", true},
		{"/", true},
		{"/var/run/../run/docker.sock", true},
		{"/var/run/docker.sock/", true},
		{"/etc", false},
		{"/run/user", false},
		{"/var/runner", false},
		{filepath.Join(root, "data"), false},
	}
	for _, tc := range tests {
		p := &Project{Services: map[string]*Service{"a": {Name: "a", Volumes: []Mount{{Type: "bind", Source: tc.source, Target: "/x"}}}}}
		got := contains(rulesOf(Check(root, p, Options{})), RuleDockerSocket)
		if got != tc.want {
			t.Errorf("source %q: docker_socket = %v, want %v", tc.source, got, tc.want)
		}
	}
}

func TestVolumeDriverOpts(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	tests := []struct {
		name string
		v    Volume
		want []string
	}{
		{"plain", Volume{}, nil},
		{"local", Volume{Driver: "local"}, nil},
		{"nfs", Volume{Driver: "local", DriverOpts: map[string]string{"type": "nfs", "o": "addr=1.2.3.4", "device": ":/x"}}, nil},
		{"external", Volume{External: true, DriverOpts: map[string]string{"o": "bind", "device": "/etc"}}, []string{RuleExternalVolume}},
		{"plugin driver", Volume{Driver: "rexray"}, []string{RuleVolumeDriverOpts}},
		{"bind inside", Volume{DriverOpts: map[string]string{"o": "bind", "type": "none", "device": filepath.Join(root, "d")}}, nil},
		{"bind outside", Volume{DriverOpts: map[string]string{"o": "bind", "type": "none", "device": "/etc"}}, []string{RuleVolumeDriverOpts}},
		{"o only", Volume{DriverOpts: map[string]string{"o": "ro,Bind", "device": "/etc"}}, []string{RuleVolumeDriverOpts}},
		{"rbind", Volume{DriverOpts: map[string]string{"o": "rbind", "device": "/etc"}}, []string{RuleVolumeDriverOpts}},
		{"type none only", Volume{DriverOpts: map[string]string{"type": "none", "device": "/etc"}}, []string{RuleVolumeDriverOpts}},
		{"no device", Volume{DriverOpts: map[string]string{"o": "bind"}}, []string{RuleVolumeDriverOpts}},
		{"relative device", Volume{DriverOpts: map[string]string{"o": "bind", "device": "data"}}, []string{RuleVolumeDriverOpts}},
		{"socket device", Volume{DriverOpts: map[string]string{"o": "bind", "device": "/var/run/docker.sock"}}, []string{RuleDockerSocket, RuleVolumeDriverOpts}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Project{Volumes: map[string]*Volume{"v": &tc.v}}
			got := rulesOf(Check(root, p, Options{}))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rules = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestConfigAndSecretFiles(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	p := &Project{
		Configs: map[string]*File{"in": {File: filepath.Join(root, "a")}, "out": {File: "/etc/passwd"}, "env": {}},
		Secrets: map[string]*File{"in": {File: filepath.Join(root, "s")}, "out": {File: "/etc/shadow"}},
	}
	got := rulesOf(Check(root, p, Options{}))
	if !reflect.DeepEqual(got, []string{RuleConfigFile, RuleSecretFile}) {
		t.Fatalf("rules = %v", got)
	}
}

func TestBadProjectRoot(t *testing.T) {
	p := &Project{Services: map[string]*Service{"a": {Name: "a", Privileged: true}}}
	vs := Check("", p, Options{})
	if len(vs) != 1 || vs[0].Rule != RuleProjectRoot {
		t.Fatalf("empty root: %+v", vs)
	}
	if vs := Check(t.TempDir(), nil, Options{}); len(vs) != 1 || vs[0].Rule != RuleUnparseable {
		t.Fatalf("nil project: %+v", vs)
	}
}

func TestOutputSorted(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	p := &Project{Services: map[string]*Service{
		"z": {Name: "z", Privileged: true, Uts: "host"},
		"a": {Name: "a", Pid: "host", Privileged: true},
		"m": {Name: "m", Cgroup: "host"},
	}}
	vs := Check(root, p, Options{})
	var got []string
	for _, v := range vs {
		got = append(got, v.Service+"/"+v.Rule)
	}
	want := []string{"a/pid", "a/privileged", "m/cgroup", "z/privileged", "z/uts"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if again := Check(root, p, Options{}); !reflect.DeepEqual(vs, again) {
		t.Fatal("non-deterministic output")
	}
}

func TestRulesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Rules {
		if seen[r] {
			t.Errorf("duplicate rule %q", r)
		}
		seen[r] = true
	}
}
