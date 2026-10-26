package config

import (
	"path/filepath"
	"testing"
)

func TestDockerConfigDirAndPull(t *testing.T) {
	d := Default()
	if d.Deploys.Pull != "always" || d.Docker.ConfigDir != "" ||
		d.Docker.ResolvedConfigDir("/var/lib/ki-agent") != "/var/lib/ki-agent/docker" {
		t.Fatalf("defaults = %+v %+v", d.Docker, d.Deploys)
	}
	cfg, err := LoadWithEnv(filepath.Join(t.TempDir(), "nope.yaml"), envMap(map[string]string{
		"KI_DOCKER_CONFIG_DIR": "/etc/ki/docker", "KI_DEPLOYS_PULL": "missing",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Docker.ResolvedConfigDir("/x") != "/etc/ki/docker" || cfg.Deploys.Pull != "missing" {
		t.Fatalf("cfg = %+v %+v", cfg.Docker, cfg.Deploys)
	}
	c := Default()
	c.Deploys.Pull = "never"
	if err := c.Validate(); err == nil {
		t.Fatal("pull=never accepted")
	}
}
