package update

import (
	"context"
	"strings"
	"testing"
)

func TestCanonical(t *testing.T) {
	tests := []struct{ in, want string }{
		{"1.0.1", "v1.0.1"},
		{"v1.0.1", "v1.0.1"},
		{"1.0.1-rc.1", "v1.0.1-rc.1"},
		{"v1.0.1-rc.1", "v1.0.1-rc.1"},
		{"vv1.0.1", "vv1.0.1"},
		{"dev", "dev"},
		{"", ""},
		{"1.0", "1.0"},
		{"v1", "v1"},
	}
	for _, tt := range tests {
		if got := Canonical(tt.in); got != tt.want {
			t.Errorf("Canonical(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsNewerWithUnprefixedBuildVersion(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"1.0.0", "v1.0.1", true},
		{"1.0.1", "v1.0.1", false},
		{"1.0.2", "v1.0.1", false},
		{"dev", "v1.0.1", false},
	}
	for _, tt := range tests {
		if got := IsNewer(tt.current, &Release{Tag: tt.latest}); got != tt.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
		}
	}
}

func TestSameVersion(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.0.2", "v1.0.2", true},
		{"v1.0.2", "v1.0.2", true},
		{"1.0.2", "1.0.2", true},
		{"1.0.1", "v1.0.2", false},
		{"dev", "v1.0.2", false},
		{"", "v1.0.2", false},
	}
	for _, tt := range tests {
		if got := sameVersion(tt.a, tt.b); got != tt.want {
			t.Errorf("sameVersion(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestApplyRefusesDowngradeFromUnprefixedCurrent(t *testing.T) {
	pub, priv := newKey(t)
	for _, tag := range []string{"v1.0.1", "v1.0.0"} {
		f := newFixture(t, &fakeRelease{tag: tag, key: priv}, pub)
		f.a.Current = "1.0.1"
		res, err := f.a.Apply(context.Background(), tag)
		if err == nil || res.Status != ResultFailed || !strings.Contains(err.Error(), "not newer") {
			t.Fatalf("%s: res = %+v, err = %v", tag, res, err)
		}
		assertUntouched(t, f)
	}
}

func TestApplyAcceptsBinaryReportingUnprefixedVersion(t *testing.T) {
	pub, priv := newKey(t)
	script := "#!/bin/sh\necho \"ki-agent 1.0.2 (commit abc)\"\n"
	f := newFixture(t, &fakeRelease{tag: "v1.0.2", key: priv, script: script}, pub)
	f.a.Current = "1.0.1"
	if res, err := f.a.Apply(context.Background(), "v1.0.2"); err != nil || res.Status != ResultSucceeded {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
}
