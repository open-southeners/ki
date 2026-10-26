package aws

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Agent is the local state of one AWS agent.
type Agent struct {
	Installed bool   `json:"installed"`
	Unit      string `json:"unit,omitempty"`
	Active    bool   `json:"active"`
	Enabled   bool   `json:"enabled"`
	Version   string `json:"version,omitempty"`
	Source    string `json:"source"` // "deb", "snap" or ""
}

// Agents groups the detected agents.
type Agents struct {
	CloudWatch Agent `json:"cloudwatch"`
	SSM        Agent `json:"ssm"`
}

const (
	unitCloudWatch = "amazon-cloudwatch-agent.service"
	unitSSMDeb     = "amazon-ssm-agent.service"
	unitSSMSnap    = "snap.amazon-ssm-agent.amazon-ssm-agent.service"

	cloudWatchVersionFile = "/opt/aws/amazon-cloudwatch-agent/bin/CWAGENT_VERSION"
	ssmSnapYAML           = "/snap/amazon-ssm-agent/current/meta/snap.yaml"
	ssmBinary             = "amazon-ssm-agent"
)

// Runner executes a command and returns its standard output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// FileReader reads a file.
type FileReader func(path string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// detector inspects local systemd units and version files.
type detector struct {
	run  Runner
	read FileReader
}

func newDetector() *detector { return &detector{run: execRunner, read: os.ReadFile} }

func (d *detector) agents(ctx context.Context) Agents {
	return Agents{CloudWatch: d.cloudWatch(ctx), SSM: d.ssm(ctx)}
}

type unitState struct {
	loaded, active, enabled bool
}

// unit queries systemd read-only. A failing or missing systemctl means the
// unit is not installed.
func (d *detector) unit(ctx context.Context, name string) unitState {
	out, err := d.run(ctx, "systemctl", "show", "-p", "LoadState,ActiveState,UnitFileState", name)
	if err != nil {
		return unitState{}
	}
	var st unitState
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "LoadState":
			st.loaded = v == "loaded"
		case "ActiveState":
			st.active = v == "active"
		case "UnitFileState":
			st.enabled = v == "enabled" || v == "enabled-runtime"
		}
	}
	return st
}

func (d *detector) cloudWatch(ctx context.Context) Agent {
	st := d.unit(ctx, unitCloudWatch)
	if !st.loaded {
		return Agent{}
	}
	a := Agent{Installed: true, Unit: unitCloudWatch, Active: st.active, Enabled: st.enabled, Source: "deb"}
	if b, err := d.read(cloudWatchVersionFile); err == nil {
		a.Version = strings.TrimSpace(string(b))
	}
	return a
}

func (d *detector) ssm(ctx context.Context) Agent {
	type cand struct {
		unit, source string
		st           unitState
	}
	cands := []cand{{unit: unitSSMSnap, source: "snap"}, {unit: unitSSMDeb, source: "deb"}}
	var pick *cand
	for i := range cands {
		c := &cands[i]
		c.st = d.unit(ctx, c.unit)
		if !c.st.loaded {
			continue
		}
		if pick == nil || (c.st.active && !pick.st.active) {
			pick = c
		}
	}
	if pick == nil {
		return Agent{}
	}
	a := Agent{Installed: true, Unit: pick.unit, Active: pick.st.active, Enabled: pick.st.enabled, Source: pick.source}
	if pick.source == "snap" {
		a.Version = d.snapVersion()
	} else {
		a.Version = d.debVersion(ctx)
	}
	return a
}

func (d *detector) snapVersion() string {
	b, err := d.read(ssmSnapYAML)
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "version:"); ok {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

var versionRe = regexp.MustCompile(`\d+(\.\d+)+`)

func (d *detector) debVersion(ctx context.Context) string {
	out, err := d.run(ctx, ssmBinary, "-version")
	if err != nil {
		return ""
	}
	return versionRe.FindString(string(out))
}
