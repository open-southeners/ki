package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ParseCompose decodes the JSON printed by `docker compose config --format
// json`. Unknown fields are ignored, but a known field with an unexpected type
// is an error so that a malformed model can never pass the policy silently.
func ParseCompose(b []byte) (*Project, error) {
	var raw struct {
		Name     string                     `json:"name"`
		Services map[string]json.RawMessage `json:"services"`
		Volumes  map[string]*rawVolume      `json:"volumes"`
		Configs  map[string]*rawFile        `json:"configs"`
		Secrets  map[string]*rawFile        `json:"secrets"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("policy: parse compose model: %w", err)
	}
	if dec.More() {
		return nil, errors.New("policy: parse compose model: trailing data")
	}
	p := &Project{
		Name:     raw.Name,
		Services: make(map[string]*Service, len(raw.Services)),
		Volumes:  map[string]*Volume{},
		Configs:  map[string]*File{},
		Secrets:  map[string]*File{},
	}
	for name, msg := range raw.Services {
		var rs rawService
		if err := json.Unmarshal(msg, &rs); err != nil {
			return nil, fmt.Errorf("policy: service %q: %w", name, err)
		}
		p.Services[name] = rs.toService(name)
	}
	for n, v := range raw.Volumes {
		vol := &Volume{}
		if v != nil {
			vol = &Volume{Driver: v.Driver, DriverOpts: v.DriverOpts, External: v.External.v}
		}
		p.Volumes[n] = vol
	}
	for n, f := range raw.Configs {
		p.Configs[n] = f.toFile()
	}
	for n, f := range raw.Secrets {
		p.Secrets[n] = f.toFile()
	}
	return p, nil
}

type rawVolume struct {
	Driver     string            `json:"driver"`
	DriverOpts map[string]string `json:"driver_opts"`
	External   flexBool          `json:"external"`
}

type rawFile struct {
	File string `json:"file"`
}

func (f *rawFile) toFile() *File {
	if f == nil {
		return &File{}
	}
	return &File{File: f.File}
}

type rawService struct {
	Privileged   bool        `json:"privileged"`
	NetworkMode  string      `json:"network_mode"`
	Pid          string      `json:"pid"`
	Ipc          string      `json:"ipc"`
	Uts          string      `json:"uts"`
	UsernsMode   string      `json:"userns_mode"`
	Cgroup       string      `json:"cgroup"`
	CgroupParent string      `json:"cgroup_parent"`
	CapAdd       []string    `json:"cap_add"`
	Devices      []rawDevice `json:"devices"`
	Gpus         presence    `json:"gpus"`
	Deploy       *struct {
		Resources *struct {
			Reservations *struct {
				Devices presence `json:"devices"`
			} `json:"reservations"`
		} `json:"resources"`
	} `json:"deploy"`
	DeviceCgroupRules []string    `json:"device_cgroup_rules"`
	SecurityOpt       []string    `json:"security_opt"`
	VolumesFrom       []string    `json:"volumes_from"`
	Sysctls           flexKV      `json:"sysctls"`
	Volumes           []rawMount  `json:"volumes"`
	Build             *rawBuild   `json:"build"`
	EnvFile           rawEnvFiles `json:"env_file"`
}

func (r *rawService) toService(name string) *Service {
	s := &Service{
		Name: name, Privileged: r.Privileged, NetworkMode: r.NetworkMode,
		Pid: r.Pid, Ipc: r.Ipc, Uts: r.Uts, UsernsMode: r.UsernsMode,
		Cgroup: r.Cgroup, CgroupParent: r.CgroupParent, CapAdd: r.CapAdd,
		DeviceCgroupRules: r.DeviceCgroupRules, SecurityOpt: r.SecurityOpt,
		VolumesFrom: r.VolumesFrom, Sysctls: r.Sysctls.m, EnvFiles: r.EnvFile.paths,
	}
	s.Gpus = int(r.Gpus)
	if r.Deploy != nil && r.Deploy.Resources != nil && r.Deploy.Resources.Reservations != nil {
		s.DeployDevices = int(r.Deploy.Resources.Reservations.Devices)
	}
	for _, d := range r.Devices {
		s.Devices = append(s.Devices, string(d))
	}
	for _, m := range r.Volumes {
		s.Volumes = append(s.Volumes, Mount(m))
	}
	if r.Build != nil {
		s.Build = &Build{Context: r.Build.Context, Dockerfile: r.Build.Dockerfile,
			SSH: sortedKeys(r.Build.SSH.m), AdditionalContexts: r.Build.AdditionalContexts.m}
	}
	return s
}

// presence counts the entries of a list, or 1 for a non-empty string or
// object (compose may emit `gpus: all`-like scalars); null and empty values
// count 0. Any other type is an error.
type presence int

func (p *presence) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case nil:
	case []any:
		*p = presence(len(t))
	case string:
		if t != "" {
			*p = 1
		}
	case map[string]any:
		if len(t) > 0 {
			*p = 1
		}
	default:
		return fmt.Errorf("unexpected value %s", b)
	}
	return nil
}

// flexBool accepts a bool or null.
type flexBool struct{ v bool }

func (f *flexBool) UnmarshalJSON(b []byte) error { return json.Unmarshal(b, &f.v) }

// rawDevice is a device entry: a short-syntax string or an object with
// source/target. Both reduce to a description string.
type rawDevice string

func (d *rawDevice) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*d = rawDevice(s)
		return nil
	}
	var o struct {
		Source string `json:"source"`
		Target string `json:"target"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("device: want string or object: %w", err)
	}
	*d = rawDevice(o.Source + ":" + o.Target)
	return nil
}

// flexKV accepts a map (values string, number, bool or null) or a list of
// "key=value" strings; used for sysctls and additional_contexts.
type flexKV struct{ m map[string]string }

func (f *flexKV) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	f.m = map[string]string{}
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var l []string
		if err := json.Unmarshal(b, &l); err != nil {
			return err
		}
		for _, e := range l {
			k, v, _ := strings.Cut(e, "=")
			f.m[k] = v
		}
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	for k, v := range m {
		var s string
		var n json.Number
		var bl bool
		switch {
		case json.Unmarshal(v, &s) == nil:
			f.m[k] = s
		case json.Unmarshal(v, &n) == nil:
			f.m[k] = n.String()
		case json.Unmarshal(v, &bl) == nil:
			f.m[k] = fmt.Sprint(bl)
		case bytes.Equal(bytes.TrimSpace(v), []byte("null")):
			f.m[k] = ""
		default:
			return fmt.Errorf("key %q: unexpected value %s", k, v)
		}
	}
	return nil
}

// rawBuild is a build section: a context string or an object.
type rawBuild struct {
	Context            string
	Dockerfile         string
	SSH                flexKV
	AdditionalContexts flexKV
}

func (r *rawBuild) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.Context = s
		return nil
	}
	var o struct {
		Context            string `json:"context"`
		Dockerfile         string `json:"dockerfile"`
		SSH                flexKV `json:"ssh"`
		AdditionalContexts flexKV `json:"additional_contexts"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("build: want string or object: %w", err)
	}
	r.Context, r.Dockerfile, r.SSH, r.AdditionalContexts = o.Context, o.Dockerfile, o.SSH, o.AdditionalContexts
	return nil
}

// rawEnvFiles is env_file: a string, or a list of strings and
// {path, required} objects.
type rawEnvFiles struct{ paths []string }

func (r *rawEnvFiles) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		r.paths = []string{s}
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		return fmt.Errorf("env_file: want string or list: %w", err)
	}
	for _, it := range items {
		if err := json.Unmarshal(it, &s); err == nil {
			r.paths = append(r.paths, s)
			continue
		}
		var o struct {
			Path *string `json:"path"`
		}
		if err := json.Unmarshal(it, &o); err != nil || o.Path == nil {
			return fmt.Errorf("env_file: unexpected entry %s", it)
		}
		r.paths = append(r.paths, *o.Path)
	}
	return nil
}

// rawMount is a volumes entry, long syntax object or short syntax string.
type rawMount Mount

func (m *rawMount) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*m = rawMount(parseShortMount(s))
		return nil
	}
	var o struct {
		Type     string   `json:"type"`
		Source   string   `json:"source"`
		Target   string   `json:"target"`
		ReadOnly flexBool `json:"read_only"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return fmt.Errorf("volume: want string or object: %w", err)
	}
	*m = rawMount{Type: o.Type, Source: o.Source, Target: o.Target, ReadOnly: o.ReadOnly.v}
	return nil
}

// parseShortMount converts "src:dst[:opts]" to a Mount. A source that looks
// like a path (absolute, ./, ../, ~) is a bind mount, otherwise a named volume.
func parseShortMount(s string) Mount {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		return Mount{Type: "volume", Target: parts[0]}
	default:
		m := Mount{Source: parts[0], Target: parts[1]}
		if strings.HasPrefix(m.Source, "/") || strings.HasPrefix(m.Source, ".") || strings.HasPrefix(m.Source, "~") {
			m.Type = "bind"
		} else {
			m.Type = "volume"
		}
		if len(parts) > 2 {
			for _, o := range strings.Split(parts[2], ",") {
				if o == "ro" {
					m.ReadOnly = true
				}
			}
		}
		return m
	}
}

// sortedKeys returns map keys in order, for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
