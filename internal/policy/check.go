package policy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Check evaluates p against the policy for a project living in projectRoot and
// returns every violation, sorted by service, rule and detail. An empty result
// means the project is allowed. projectRoot is resolved (symlinks included)
// before any containment test.
func Check(projectRoot string, p *Project, opts Options) []Violation {
	c := &checker{opts: opts, p: p}
	if p == nil {
		return []Violation{{Rule: RuleUnparseable, Detail: "no project model"}}
	}
	absRoot, err := filepath.Abs(projectRoot)
	if err == nil {
		c.root, err = resolvePath(absRoot)
	}
	if err != nil || projectRoot == "" {
		c.add("", RuleProjectRoot, fmt.Sprintf("cannot resolve project root %q: %v", projectRoot, err))
		return c.sorted()
	}
	c.allowed = map[string]bool{}
	caps := opts.AllowedCapabilities
	if caps == nil {
		caps = DefaultAllowedCapabilities
	}
	for _, cp := range caps {
		if n := normCap(cp); n != "ALL" {
			c.allowed[n] = true
		}
	}

	for _, name := range sortedKeys(p.Services) {
		if s := p.Services[name]; s != nil {
			c.service(name, s)
		}
	}
	for _, name := range sortedKeys(p.Volumes) {
		c.volume(name, p.Volumes[name])
	}
	for _, name := range sortedKeys(p.Configs) {
		if f := p.Configs[name]; f != nil && f.File != "" {
			c.contained("", RuleConfigFile, fmt.Sprintf("config %q file", name), f.File)
		}
	}
	for _, name := range sortedKeys(p.Secrets) {
		if f := p.Secrets[name]; f != nil && f.File != "" {
			c.contained("", RuleSecretFile, fmt.Sprintf("secret %q file", name), f.File)
		}
	}
	return c.sorted()
}

type checker struct {
	opts    Options
	p       *Project
	root    string
	allowed map[string]bool
	out     []Violation
}

func (c *checker) add(service, rule, detail string) {
	c.out = append(c.out, Violation{Service: service, Rule: rule, Detail: detail})
}

func (c *checker) sorted() []Violation {
	sort.SliceStable(c.out, func(i, j int) bool {
		a, b := c.out[i], c.out[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Detail < b.Detail
	})
	return c.out
}

func (c *checker) service(name string, s *Service) {
	if s.Privileged {
		c.add(name, RulePrivileged, "privileged mode is not allowed")
	}
	c.nsMode(name, RuleNetworkMode, "network_mode", s.NetworkMode, "host")
	c.nsMode(name, RulePid, "pid", s.Pid, "host")
	c.nsMode(name, RuleIpc, "ipc", s.Ipc, "host")
	if strings.EqualFold(strings.TrimSpace(s.Uts), "host") {
		c.add(name, RuleUts, "uts: host is not allowed")
	}
	if strings.EqualFold(strings.TrimSpace(s.UsernsMode), "host") {
		c.add(name, RuleUsernsMode, "userns_mode: host is not allowed")
	}
	if strings.EqualFold(strings.TrimSpace(s.Cgroup), "host") {
		c.add(name, RuleCgroup, "cgroup: host is not allowed")
	}
	for _, cp := range s.CapAdd {
		n := normCap(cp)
		switch {
		case n == "ALL":
			c.add(name, RuleCapAdd, "cap_add ALL is not allowed")
		case !c.allowed[n]:
			c.add(name, RuleCapAdd, fmt.Sprintf("capability %s is not allowed", n))
		}
	}
	for i := 0; i < s.Gpus; i++ {
		c.add(name, RuleDevices, "gpus reservation is not allowed")
	}
	for i := 0; i < s.DeployDevices; i++ {
		c.add(name, RuleDevices, "deploy.resources.reservations.devices is not allowed")
	}
	for _, d := range s.Devices {
		c.add(name, RuleDevices, fmt.Sprintf("device %q is not allowed", d))
	}
	for _, r := range s.DeviceCgroupRules {
		c.add(name, RuleDeviceCgroupRules, fmt.Sprintf("device cgroup rule %q is not allowed", r))
	}
	for _, o := range s.SecurityOpt {
		if unsafeSecurityOpt(o) {
			c.add(name, RuleSecurityOpt, fmt.Sprintf("security_opt %q is not allowed", o))
		}
	}
	if s.CgroupParent != "" {
		c.add(name, RuleCgroupParent, fmt.Sprintf("cgroup_parent %q is not allowed", s.CgroupParent))
	}
	for _, v := range s.VolumesFrom {
		c.add(name, RuleVolumesFrom, fmt.Sprintf("volumes_from %q is not allowed", v))
	}
	if s.Provider {
		c.add(name, RuleProvider, "provider services are not allowed")
	}
	if rt := strings.TrimSpace(s.Runtime); rt != "" && rt != "runc" {
		c.add(name, RuleRuntime, fmt.Sprintf("runtime %q is not allowed", s.Runtime))
	}
	for _, k := range sortedKeys(s.Labels) {
		if reservedLabel(k) {
			c.add(name, RuleReservedLabel, fmt.Sprintf("label %q is reserved", k))
		}
	}
	for _, k := range sortedKeys(s.Sysctls) {
		if !strings.HasPrefix(k, "net.") {
			c.add(name, RuleSysctls, fmt.Sprintf("sysctl %q is not allowed (only net.* is)", k))
		}
	}
	for _, m := range s.Volumes {
		c.mount(name, m)
	}
	if s.Build != nil {
		c.buildContext(name, "build.context", s.Build.Context)
		c.dockerfile(name, s.Build)
		for _, k := range s.Build.SSH {
			c.add(name, RuleBuildSSH, fmt.Sprintf("build.ssh %q forwards the host SSH agent", k))
		}
		for _, k := range sortedKeys(s.Build.AdditionalContexts) {
			c.buildContext(name, fmt.Sprintf("build.additional_contexts.%s", k), s.Build.AdditionalContexts[k])
		}
	}
	for _, f := range s.EnvFiles {
		c.contained(name, RuleEnvFile, "env_file", f)
	}
}

// nsMode checks a namespace-sharing mode. Values in bad (case-insensitive) and
// container:/ns: references are rejected; none, bridge and custom networks pass; service:<svc> must name a service of
// this project.
func (c *checker) nsMode(service, rule, field, v string, bad ...string) {
	l := strings.ToLower(strings.TrimSpace(v))
	if l == "" {
		return
	}
	for _, b := range bad {
		if l == b {
			c.add(service, rule, fmt.Sprintf("%s: %s is not allowed", field, v))
			return
		}
	}
	switch {
	case strings.HasPrefix(l, "container:"), strings.HasPrefix(l, "ns:"):
		c.add(service, rule, fmt.Sprintf("%s: %s joins a namespace outside the project", field, v))
	case strings.HasPrefix(l, "service:"):
		ref := strings.TrimSpace(v[len("service:"):])
		if _, ok := c.p.Services[ref]; !ok {
			c.add(service, rule, fmt.Sprintf("%s: %s references a service outside the project", field, v))
		}
	}
}

func (c *checker) mount(service string, m Mount) {
	switch m.Type {
	case "tmpfs", "image", "npipe":
		return
	case "volume":
		// Named and anonymous volumes are managed by the engine; see volume().
		// A volume "source" that is a path is a host path in disguise.
		if !strings.Contains(m.Source, "/") {
			return
		}
	case "bind":
		if m.Source == "" {
			c.add(service, RuleUnparseable, fmt.Sprintf("bind mount to %q has no source", m.Target))
			return
		}
	default:
		c.add(service, RuleUnparseable, fmt.Sprintf("unknown mount type %q for %q", m.Type, m.Target))
		return
	}
	src := m.Source
	if !filepath.IsAbs(src) {
		c.add(service, RuleBindMount, fmt.Sprintf("bind source %q is not an absolute path", src))
		return
	}
	clean := filepath.Clean(src)
	real, err := resolvePath(clean)
	if err != nil {
		c.add(service, RuleBindMount, fmt.Sprintf("bind source %q cannot be resolved: %v", src, err))
		real = ""
	} else if !within(c.root, real) {
		c.add(service, RuleBindMount, fmt.Sprintf("bind source %q resolves to %q outside the project root", src, real))
	}
	if touchesEngineSocketVia(clean, real) {
		c.add(service, RuleDockerSocket, fmt.Sprintf("mount of %q exposes the container engine socket", src))
	}
}

func (c *checker) volume(name string, v *Volume) {
	if v == nil {
		return
	}
	if v.External {
		c.add("", RuleExternalVolume, fmt.Sprintf("volume %q is external; it may hold another project's data", name))
		return
	}
	if v.Driver != "" && v.Driver != "local" {
		c.add("", RuleVolumeDriverOpts, fmt.Sprintf("volume %q uses driver %q; only the local driver is allowed", name, v.Driver))
		return
	}
	o := strings.ToLower(v.DriverOpts["o"])
	typ := strings.ToLower(v.DriverOpts["type"])
	bind := typ == "none" || typ == "bind"
	for _, f := range strings.Split(o, ",") {
		if f = strings.TrimSpace(f); f == "bind" || f == "rbind" {
			bind = true
		}
	}
	if !bind {
		return
	}
	dev := v.DriverOpts["device"]
	if dev == "" {
		c.add("", RuleVolumeDriverOpts, fmt.Sprintf("volume %q is a bind volume without a device", name))
		return
	}
	c.contained("", RuleVolumeDriverOpts, fmt.Sprintf("volume %q device", name), dev)
	r, _ := resolvePath(dev)
	if filepath.IsAbs(dev) && touchesEngineSocketVia(dev, r) {
		c.add("", RuleDockerSocket, fmt.Sprintf("volume %q device exposes the container engine socket", name))
	}
}

func (c *checker) buildContext(service, what, ctx string) {
	if ctx == "" || isRemoteContext(ctx) {
		return
	}
	c.contained(service, RuleBuildContext, what, ctx)
}

// dockerfile checks that a local build's Dockerfile (relative paths are
// relative to the context) lies inside the project root.
func (c *checker) dockerfile(service string, b *Build) {
	if b.Dockerfile == "" || b.Context == "" || isRemoteContext(b.Context) {
		return
	}
	df := b.Dockerfile
	if !filepath.IsAbs(df) {
		if !filepath.IsAbs(b.Context) {
			c.add(service, RuleBuildContext, fmt.Sprintf("build.dockerfile %q has a relative context", df))
			return
		}
		df = filepath.Join(b.Context, df)
	}
	c.contained(service, RuleBuildContext, "build.dockerfile", df)
}

// contained reports a violation of rule unless path is an absolute path that
// resolves inside the project root.
func (c *checker) contained(service, rule, what, path string) {
	if !filepath.IsAbs(path) {
		c.add(service, rule, fmt.Sprintf("%s %q is not an absolute path", what, path))
		return
	}
	real, err := resolvePath(path)
	if err != nil {
		c.add(service, rule, fmt.Sprintf("%s %q cannot be resolved: %v", what, path, err))
		return
	}
	if !within(c.root, real) {
		c.add(service, rule, fmt.Sprintf("%s %q resolves to %q outside the project root", what, path, real))
	}
}

func normCap(s string) string {
	return strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "CAP_")
}

// unsafeSecurityOpt reports options that disable confinement. Both the
// key=value and legacy key:value spellings are recognised.
func unsafeSecurityOpt(o string) bool {
	n := strings.ToLower(strings.Join(strings.Fields(o), ""))
	if !strings.Contains(n, "=") {
		n = strings.Replace(n, ":", "=", 1)
	}
	switch n {
	case "seccomp=unconfined", "apparmor=unconfined", "systempaths=unconfined",
		"no-new-privileges=false", "label=disable", "label=type:spc_t":
		return true
	}
	return false
}

// allowedKiLabels are the only `ki.` labels a project may set.
var allowedKiLabels = map[string]bool{
	"ki.expose.port": true, "ki.expose.domains": true, "ki.expose.path": true,
}

// reservedLabel reports whether a service label key is in a namespace the
// agent owns. The prefixes match case-insensitively (and ignoring surrounding
// space), so `Traefik.x` and `KI.foo` are reserved; the allowed ki.expose.*
// keys must match exactly.
func reservedLabel(key string) bool {
	k := strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(k, "traefik.") {
		return true
	}
	return strings.HasPrefix(k, "ki.") && !allowedKiLabels[key]
}
