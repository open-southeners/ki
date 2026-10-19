package git

import (
	"errors"
	"strings"
	"testing"
)

func TestParseURLValid(t *testing.T) {
	all := []string{"https", "ssh", "file"}
	tests := []struct {
		raw     string
		allowed []string
		want    URL
	}{
		{"https://github.com/acme/app.git", all, URL{Scheme: "https", Host: "github.com", Path: "/acme/app.git", Name: "app"}},
		{"https://example.com:8443/a/b/", all, URL{Scheme: "https", Host: "example.com:8443", Path: "/a/b/", Name: "b"}},
		{"ssh://git@host.example/acme/app", all, URL{Scheme: "ssh", Host: "host.example", Path: "/acme/app", Name: "app"}},
		{"ssh://host:2222/acme/app.git", all, URL{Scheme: "ssh", Host: "host:2222", Path: "/acme/app.git", Name: "app"}},
		{"git@github.com:acme/app.git", all, URL{Scheme: "ssh", Host: "github.com", Path: "acme/app.git", Name: "app"}},
		{"git@host:/srv/repos/x.git", all, URL{Scheme: "ssh", Host: "host", Path: "/srv/repos/x.git", Name: "x"}},
		{"http://localhost:3000/acme/app.git", all, URL{Scheme: "http", Host: "localhost:3000", Path: "/acme/app.git", Name: "app"}},
		{"http://127.0.0.1/acme/app", all, URL{Scheme: "http", Host: "127.0.0.1", Path: "/acme/app", Name: "app"}},
		{"http://[::1]:80/acme/app", all, URL{Scheme: "http", Host: "[::1]:80", Path: "/acme/app", Name: "app"}},
		{"file:///tmp/repos/x.git", all, URL{Scheme: "file", Path: "/tmp/repos/x.git", Name: "x"}},
	}
	for _, tc := range tests {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := ParseURL(tc.raw, tc.allowed)
			if err != nil {
				t.Fatalf("ParseURL: %v", err)
			}
			tc.want.Raw = tc.raw
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseURLRejected(t *testing.T) {
	def := []string{"https", "ssh"}
	tests := []struct {
		name, raw string
		allowed   []string
		detail    string
	}{
		{"empty", "", def, ""},
		{"space", "https://github.com/a b/c", def, ""},
		{"newline", "https://github.com/a/c\n", def, ""},
		{"control", "https://github.com/a/c\x00", def, ""},
		{"leading dash", "-oProxyCommand=x", def, ""},
		{"dash host", "ssh://-oProxyCommand=x/a/b", def, ""},
		{"dash scp host", "git@-host:a/b", def, ""},
		{"dash scp user", "-ofoo@host:a/b", def, ""},
		{"dash segment", "https://github.com/acme/-evil", def, ""},
		{"dash scp segment", "git@host:acme/-evil", def, ""},
		{"ext transport", "ext::sh -c id", def, ""},
		{"ext transport nospace", "ext::sh%20id", def, ""},
		{"fd transport", "fd::17/foo", def, ""},
		{"local abs path", "/srv/repos/x.git", def, ""},
		{"local rel path", "../x.git", def, ""},
		{"scp without user", "host:acme/app", def, ""},
		{"credentials", "https://user:pass@github.com/a/b", def, "token"},
		{"user only", "https://tok@github.com/a/b", def, "token"},
		{"ssh password", "ssh://u:p@host/a/b", def, ""},
		{"plain http remote", "http://example.com/a/b", def, ""},
		{"file not allowed", "file:///tmp/x.git", def, ""},
		{"file relative", "file://tmp/x.git", []string{"file"}, ""},
		{"file host", "file://evil/tmp/x.git", []string{"file"}, ""},
		{"ssh not allowed", "ssh://host/a/b", []string{"https"}, ""},
		{"scp not allowed", "git@host:a/b", []string{"https"}, ""},
		{"unknown scheme", "git://github.com/a/b", def, ""},
		{"ftp", "ftp://host/a/b", def, ""},
		{"no host", "https:///a/b", def, ""},
		{"no path", "https://github.com", def, ""},
		{"query", "https://github.com/a/b?x=1", def, ""},
		{"fragment", "https://github.com/a/b#x", def, ""},
		{"bad host", "https://git_hub.com/a/b", def, ""},
		{"backslash", `https://github.com\@evil.com/a/b`, def, ""},
		{"no name", "https://github.com/.git", def, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseURL(tc.raw, tc.allowed)
			var ge *Error
			if !errors.As(err, &ge) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if ge.Reason != ReasonInvalidURL {
				t.Errorf("reason = %q, want %q", ge.Reason, ReasonInvalidURL)
			}
			if !strings.Contains(ge.Detail, tc.detail) {
				t.Errorf("detail = %q, want it to contain %q", ge.Detail, tc.detail)
			}
		})
	}
}
