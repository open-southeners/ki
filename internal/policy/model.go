// Package policy is the safety guard for container projects.
//
// Access to the Docker socket is root-equivalent on the host, so before a
// project's Compose file is ever started it is checked here: anything that
// would let a service escape its project folder (host namespaces, privileged
// mode, devices, bind mounts outside the project root, the container-engine
// socket, ...) is reported as a [Violation].
//
// The input is the normalized model printed by
// `docker compose config --format json --no-env-resolution` (Compose v2),
// in which Compose has already merged overrides, interpolated variables and
// rewritten every relative path to an absolute one. The package is a pure
// library: it never talks to a daemon, it only reads the filesystem to resolve
// symlinks while checking path containment.
//
// Everything is fail-closed: a field with an unexpected type makes
// [ParseCompose] return an error, and a value the checks cannot interpret
// (relative host path, unresolvable symlink, unknown mount type) is reported
// as a violation instead of being waved through.
package policy

// Project is the normalized Compose project.
type Project struct {
	Name     string
	Services map[string]*Service
	// Volumes, Configs and Secrets are the top-level definitions.
	Volumes map[string]*Volume
	Configs map[string]*File
	Secrets map[string]*File
}

// Service is the security-relevant subset of a Compose service.
type Service struct {
	Name              string
	Privileged        bool
	NetworkMode       string
	Pid               string
	Ipc               string
	Uts               string
	UsernsMode        string
	Cgroup            string
	CgroupParent      string
	CapAdd            []string
	Devices           []string
	Gpus              int // number of `gpus` entries
	DeployDevices     int // number of deploy.resources.reservations.devices entries
	DeviceCgroupRules []string
	SecurityOpt       []string
	VolumesFrom       []string
	Sysctls           map[string]string
	Volumes           []Mount
	Build             *Build
	EnvFiles          []string
}

// Mount is one entry of a service's volumes in long syntax. Short-syntax
// strings are converted by [ParseCompose].
type Mount struct {
	Type     string // bind, volume, tmpfs, image, npipe
	Source   string
	Target   string
	ReadOnly bool
}

// Build is a service build section.
type Build struct {
	Context            string
	Dockerfile         string
	SSH                []string
	AdditionalContexts map[string]string
}

// Volume is a top-level volume definition.
type Volume struct {
	Driver     string
	DriverOpts map[string]string
	External   bool
}

// File is a top-level config or secret definition; File is empty when the
// content comes from the environment, inline content or an external object.
type File struct {
	File string
}

// Violation is one policy breach. Service is empty for project-level
// definitions (top-level volumes, configs, secrets) and for problems with the
// project root itself.
type Violation struct {
	Service string `json:"service"`
	Rule    string `json:"rule"`
	Detail  string `json:"detail"`
}

// Options tunes [Check].
type Options struct {
	// AllowedCapabilities lists the capabilities `cap_add` may request, with
	// or without the CAP_ prefix. nil selects [DefaultAllowedCapabilities]; an
	// empty non-nil slice allows none. ALL is never allowed.
	AllowedCapabilities []string
}

// DefaultAllowedCapabilities is used when Options.AllowedCapabilities is nil.
var DefaultAllowedCapabilities = []string{"NET_BIND_SERVICE"}

// Stable rule identifiers. They appear in API responses, so never rename them.
const (
	// RulePrivileged rejects `privileged: true`: full device access and no
	// confinement, a trivial host escape.
	RulePrivileged = "privileged"
	// RuleNetworkMode rejects `network_mode` host (shares the host network
	// stack and reaches host-only services), `container:<x>` (joins a
	// container outside the project) and `ns:` paths. `service:<svc>` is
	// allowed only for a service of the same project; none, bridge and custom
	// networks are fine.
	RuleNetworkMode = "network_mode"
	// RulePid rejects `pid: host` (sees and signals host processes) and
	// `pid: container:<x>` (joins a foreign container's PID namespace).
	RulePid = "pid"
	// RuleIpc rejects `ipc: host` (host shared memory) and `container:<x>`.
	RuleIpc = "ipc"
	// RuleUts rejects `uts: host` (lets the container change the host name).
	RuleUts = "uts"
	// RuleUsernsMode rejects `userns_mode: host`, which disables user
	// namespace remapping so container root is host root.
	RuleUsernsMode = "userns_mode"
	// RuleCgroup rejects `cgroup: host`, giving write access to the host
	// cgroup hierarchy (a known escape via release_agent).
	RuleCgroup = "cgroup"
	// RuleCapAdd rejects capabilities outside [Options.AllowedCapabilities];
	// ALL is always rejected. Extra capabilities such as SYS_ADMIN or
	// SYS_PTRACE are the usual stepping stones to escape.
	RuleCapAdd = "cap_add"
	// RuleDevices rejects any `devices` entry, `gpus` entry or
	// deploy.resources.reservations.devices reservation: host device nodes
	// bypass filesystem isolation.
	RuleDevices = "devices"
	// RuleDeviceCgroupRules rejects `device_cgroup_rules`, which grant
	// access to arbitrary device major/minor numbers.
	RuleDeviceCgroupRules = "device_cgroup_rules"
	// RuleSecurityOpt rejects options that switch off confinement:
	// seccomp=unconfined, apparmor=unconfined, label=disable,
	// label=type:spc_t, systempaths=unconfined and no-new-privileges=false.
	RuleSecurityOpt = "security_opt"
	// RuleCgroupParent rejects `cgroup_parent`, which places the container in
	// an arbitrary host cgroup and so can dodge resource accounting.
	RuleCgroupParent = "cgroup_parent"
	// RuleVolumesFrom rejects `volumes_from`, which inherits every mount of
	// another container (possibly one outside this project).
	RuleVolumesFrom = "volumes_from"
	// RuleBindMount rejects bind mounts whose source, after symlink
	// resolution, is outside the project root. Relative or otherwise
	// uninterpretable sources are rejected too.
	RuleBindMount = "bind_mount"
	// RuleDockerSocket rejects any mount of the Docker, containerd or podman
	// socket (or of a parent directory of one), even inside the project root:
	// the engine socket is root on the host.
	RuleDockerSocket = "docker_socket"
	// RuleVolumeDriverOpts rejects top-level local volumes that are really
	// bind mounts (`o: bind`, `type: none`) of a `device` outside the project
	// root (the classic named-volume escape), and any driver other than
	// local: volume plugins can mount arbitrary host or remote storage and
	// their options cannot be judged here.
	// RuleExternalVolume rejects `external: true` volumes: they are created
	// outside the project and may hold another project's data.
	RuleVolumeDriverOpts = "volume_driver_opts"
	RuleExternalVolume   = "external_volume"
	// RuleBuildSSH rejects `build.ssh` (any value, including default), which
	// forwards the host's SSH agent into the build.
	RuleBuildSSH = "build_ssh"
	// RuleBuildContext rejects a build context, additional context or
	// Dockerfile (relative ones are taken relative to the context) that is a
	// local path outside the project root. Remote contexts are fine. Files
	// behind build.secrets are top-level secrets, covered by RuleSecretFile.
	RuleBuildContext = "build_context"
	// RuleEnvFile rejects `env_file` paths outside the project root.
	RuleEnvFile = "env_file"
	// RuleConfigFile rejects top-level `configs.<x>.file` outside the root.
	RuleConfigFile = "config_file"
	// RuleSecretFile rejects top-level `secrets.<x>.file` outside the root.
	RuleSecretFile = "secret_file"
	// RuleSysctls rejects sysctls outside the namespaced `net.*` family;
	// everything else is host-global or dangerous.
	RuleSysctls = "sysctls"
	// RuleProjectRoot is reported when the project root itself cannot be
	// resolved, in which case no containment check can be trusted.
	RuleProjectRoot = "project_root"
	// RuleUnparseable marks a value the checks cannot interpret (for example
	// an unknown mount type). Treated as a violation, never as a pass.
	RuleUnparseable = "unparseable"
)

// Rules lists every rule id, for documentation and exhaustiveness tests.
var Rules = []string{
	RulePrivileged, RuleNetworkMode, RulePid, RuleIpc, RuleUts, RuleUsernsMode,
	RuleCgroup, RuleCapAdd, RuleDevices, RuleDeviceCgroupRules, RuleSecurityOpt,
	RuleCgroupParent, RuleVolumesFrom, RuleBindMount, RuleDockerSocket,
	RuleVolumeDriverOpts, RuleExternalVolume, RuleBuildSSH, RuleBuildContext, RuleEnvFile, RuleConfigFile,
	RuleSecretFile, RuleSysctls, RuleProjectRoot, RuleUnparseable,
}
