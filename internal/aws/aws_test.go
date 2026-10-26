package aws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const fakeToken = "tok-123"

// fakeIMDS serves the metadata paths. v1 allows untokened GETs, noIAM makes
// iam/info answer 404.
func fakeIMDS(t *testing.T, v1, noIAM bool) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" {
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(fakeToken))
			return
		}
		if !v1 && r.Header.Get("X-aws-ec2-metadata-token") != fakeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch strings.TrimPrefix(r.URL.Path, "/latest/meta-data/") {
		case "instance-id":
			_, _ = w.Write([]byte("i-0123456789abcdef0"))
		case "instance-type":
			_, _ = w.Write([]byte("t3.small"))
		case "placement/region":
			_, _ = w.Write([]byte("eu-west-1"))
		case "placement/availability-zone":
			_, _ = w.Write([]byte("eu-west-1a"))
		case "ami-id":
			_, _ = w.Write([]byte("ami-0123456789abcdef0"))
		case "iam/info":
			if noIAM {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(`{"Code":"Success","InstanceProfileArn":"arn:aws:iam::123456789012:instance-profile/web","InstanceProfileId":"AIPA"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

func TestIMDSv2Only(t *testing.T) {
	srv, paths := fakeIMDS(t, false, false)
	id := NewIMDS(srv.URL, nil).Identity(context.Background())
	if !id.Detected || id.InstanceID != "i-0123456789abcdef0" || id.InstanceType != "t3.small" ||
		id.Region != "eu-west-1" || id.AvailabilityZone != "eu-west-1a" || id.AMIID != "ami-0123456789abcdef0" {
		t.Fatalf("identity = %+v", id)
	}
	if id.InstanceProfileARN != "arn:aws:iam::123456789012:instance-profile/web" {
		t.Errorf("arn = %q", id.InstanceProfileARN)
	}
	if id.IMDSv1Enabled == nil || *id.IMDSv1Enabled {
		t.Errorf("imds v1 = %v, want false", id.IMDSv1Enabled)
	}
	for _, p := range *paths {
		if strings.Contains(p, "security-credentials") {
			t.Errorf("requested %s", p)
		}
	}
}

func TestIMDSv1Open(t *testing.T) {
	srv, _ := fakeIMDS(t, true, false)
	id := NewIMDS(srv.URL, nil).Identity(context.Background())
	if !id.Detected || id.IMDSv1Enabled == nil || !*id.IMDSv1Enabled {
		t.Fatalf("identity = %+v", id)
	}
}

func TestIMDSNoInstanceProfile(t *testing.T) {
	srv, _ := fakeIMDS(t, false, true)
	id := NewIMDS(srv.URL, nil).Identity(context.Background())
	if !id.Detected || id.InstanceProfileARN != "" {
		t.Fatalf("identity = %+v", id)
	}
}

func TestIMDSNotEC2FastAndCached(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewIMDS(srv.URL, nil)
	now := time.Now()
	c.now = func() time.Time { return now }

	start := time.Now()
	if id := c.Identity(context.Background()); id.Detected {
		t.Fatalf("detected = %+v", id)
	}
	if time.Since(start) > time.Second {
		t.Error("not-EC2 answer too slow")
	}
	n := hits.Load()
	c.Identity(context.Background())
	if hits.Load() != n {
		t.Error("negative answer not cached")
	}
	now = now.Add(identityTTL + time.Second)
	c.Identity(context.Background())
	if hits.Load() == n {
		t.Error("cache did not expire")
	}
}

func TestIMDSUnreachableTimesOut(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	c := NewIMDS(srv.URL, &http.Client{Timeout: 100 * time.Millisecond})
	start := time.Now()
	if c.Identity(context.Background()).Detected {
		t.Fatal("detected")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("too slow")
	}
}

func TestIMDSAllowlist(t *testing.T) {
	srv, paths := fakeIMDS(t, true, false)
	c := NewIMDS(srv.URL, nil)
	for _, p := range []string{"iam/security-credentials/", "iam/security-credentials/role", "user-data", "../x"} {
		_, err := c.get(context.Background(), fakeToken, p)
		if !errors.Is(err, ErrPathNotAllowed) {
			t.Errorf("get(%q) err = %v", p, err)
		}
	}
	if len(*paths) != 0 {
		t.Errorf("requests were made: %v", *paths)
	}
}

// runner builds a fake systemctl: units maps unit -> "Load Active File".
func runner(units map[string][3]string, binOut string) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == ssmBinary {
			if binOut == "" {
				return nil, errors.New("not found")
			}
			return []byte(binOut), nil
		}
		unit := args[len(args)-1]
		s, ok := units[unit]
		if !ok {
			s = [3]string{"not-found", "inactive", ""}
		}
		return []byte("LoadState=" + s[0] + "\nActiveState=" + s[1] + "\nUnitFileState=" + s[2] + "\n"), nil
	}
}

func files(m map[string]string) FileReader {
	return func(p string) ([]byte, error) {
		if v, ok := m[p]; ok {
			return []byte(v), nil
		}
		return nil, errors.New("missing")
	}
}

func TestAgentDetection(t *testing.T) {
	snapYAML := "name: amazon-ssm-agent\nversion: 3.3.0.0\n"
	tests := []struct {
		name  string
		units map[string][3]string
		bin   string
		files map[string]string
		ssm   Agent
		cw    Agent
	}{
		{
			name:  "deb",
			units: map[string][3]string{unitSSMDeb: {"loaded", "active", "enabled"}},
			bin:   "SSM Agent version: 3.2.582.0\n",
			ssm:   Agent{Installed: true, Unit: unitSSMDeb, Active: true, Enabled: true, Version: "3.2.582.0", Source: "deb"},
		},
		{
			name:  "snap",
			units: map[string][3]string{unitSSMSnap: {"loaded", "active", "enabled"}},
			files: map[string]string{ssmSnapYAML: snapYAML},
			ssm:   Agent{Installed: true, Unit: unitSSMSnap, Active: true, Enabled: true, Version: "3.3.0.0", Source: "snap"},
		},
		{
			name:  "both prefers active",
			units: map[string][3]string{unitSSMSnap: {"loaded", "inactive", "disabled"}, unitSSMDeb: {"loaded", "active", "enabled"}},
			bin:   "SSM Agent version: 3.2.1.0",
			ssm:   Agent{Installed: true, Unit: unitSSMDeb, Active: true, Enabled: true, Version: "3.2.1.0", Source: "deb"},
		},
		{
			name: "both absent",
		},
		{
			name:  "inactive and disabled",
			units: map[string][3]string{unitSSMSnap: {"loaded", "inactive", "disabled"}},
			files: map[string]string{ssmSnapYAML: snapYAML},
			ssm:   Agent{Installed: true, Unit: unitSSMSnap, Version: "3.3.0.0", Source: "snap"},
		},
		{
			name:  "cloudwatch",
			units: map[string][3]string{unitCloudWatch: {"loaded", "active", "enabled"}},
			files: map[string]string{cloudWatchVersionFile: "1.300031.0b313\n"},
			cw:    Agent{Installed: true, Unit: unitCloudWatch, Active: true, Enabled: true, Version: "1.300031.0b313", Source: "deb"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &detector{run: runner(tc.units, tc.bin), read: files(tc.files)}
			got := d.agents(context.Background())
			if got.SSM != tc.ssm {
				t.Errorf("ssm = %+v, want %+v", got.SSM, tc.ssm)
			}
			if got.CloudWatch != tc.cw {
				t.Errorf("cloudwatch = %+v, want %+v", got.CloudWatch, tc.cw)
			}
		})
	}
}

func TestSystemctlFailureMeansAbsent(t *testing.T) {
	d := &detector{
		run:  func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no systemctl") },
		read: files(nil),
	}
	got := d.agents(context.Background())
	if got.SSM.Installed || got.CloudWatch.Installed {
		t.Errorf("agents = %+v", got)
	}
}

func TestServiceStatus(t *testing.T) {
	srv, _ := fakeIMDS(t, false, false)
	s := NewWith(NewIMDS(srv.URL, nil), runner(nil, ""), files(nil))
	res := s.Status(context.Background())
	if !res.EC2.Detected || res.Agents.SSM.Installed {
		t.Fatalf("result = %+v", res)
	}
}
