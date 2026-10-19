// Package git wraps the git command line for cloning and updating project
// repositories whose URLs come from API callers.
//
// Every URL is validated before use, every subprocess runs with an explicit
// minimal environment, credentials never appear in argv, URLs or
// .git/config, and temporary key files are removed after each call.
package git

import (
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// URL is a validated repository URL.
type URL struct {
	// Raw is the URL exactly as given.
	Raw string
	// Scheme is "https", "ssh", "http" or "file"; scp-like URLs are "ssh".
	Scheme string
	// Host is the host, including any port; empty for file URLs.
	Host string
	// Path is the repository path.
	Path string
	// Name is the last path segment without a ".git" suffix.
	Name string
}

var (
	transportRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*::`)
	schemeRe    = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*)://`)
	scpRe       = regexp.MustCompile(`^([A-Za-z0-9._][A-Za-z0-9._-]*)@([A-Za-z0-9][A-Za-z0-9.-]*):(.+)$`)
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)
	userRe      = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._-]*$`)
)

func invalidURL(detail string) error {
	return &Error{Reason: ReasonInvalidURL, Detail: detail}
}

// ParseURL validates raw and splits it into its parts. Only schemes listed in
// allowed are accepted ("https", "ssh", "file"); plain http is accepted for
// loopback hosts when "http" or "https" is allowed. scp-like
// "user@host:path" URLs are ssh. Failures are *Error with ReasonInvalidURL.
func ParseURL(raw string, allowed []string) (URL, error) {
	if raw == "" {
		return URL{}, invalidURL("repository URL is empty")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f || r == '\\' {
			return URL{}, invalidURL("repository URL contains whitespace, control or backslash characters")
		}
	}
	if transportRe.MatchString(raw) {
		return URL{}, invalidURL("transport helper URLs are not allowed")
	}
	if strings.HasPrefix(raw, "-") {
		return URL{}, invalidURL("repository URL must not start with '-'")
	}

	var u URL
	var err error
	if m := schemeRe.FindStringSubmatch(raw); m != nil {
		u, err = parseSchemeURL(raw, strings.ToLower(m[1]), allowed)
	} else if m := scpRe.FindStringSubmatch(raw); m != nil {
		if !slices.Contains(allowed, "ssh") {
			return URL{}, invalidURL("ssh URLs are not allowed")
		}
		u = URL{Raw: raw, Scheme: "ssh", Host: m[2], Path: m[3]}
	} else {
		return URL{}, invalidURL("not a supported repository URL (use https://, ssh:// or user@host:path)")
	}
	if err != nil {
		return URL{}, err
	}

	for seg := range strings.SplitSeq(u.Path, "/") {
		if strings.HasPrefix(seg, "-") {
			return URL{}, invalidURL("repository path segments must not start with '-'")
		}
	}
	segs := strings.Split(strings.TrimRight(u.Path, "/"), "/")
	u.Name = strings.TrimSuffix(segs[len(segs)-1], ".git")
	if u.Name == "" || u.Name == "." || u.Name == ".." {
		return URL{}, invalidURL("repository URL has no repository name")
	}
	return u, nil
}

func parseSchemeURL(raw, scheme string, allowed []string) (URL, error) {
	switch scheme {
	case "https", "ssh", "http", "file":
	default:
		return URL{}, invalidURL("unsupported URL scheme " + scheme)
	}
	allow := slices.Contains(allowed, scheme)
	if scheme == "http" {
		allow = allow || slices.Contains(allowed, "https")
	}
	if !allow {
		return URL{}, invalidURL(scheme + " URLs are not allowed")
	}
	pu, err := url.Parse(raw)
	if err != nil {
		return URL{}, invalidURL("malformed repository URL")
	}
	if pu.RawQuery != "" || pu.Fragment != "" || pu.ForceQuery || strings.Contains(raw, "#") {
		return URL{}, invalidURL("repository URL must not have a query or fragment")
	}
	u := URL{Raw: raw, Scheme: scheme, Path: pu.Path}

	if scheme == "file" {
		if pu.User != nil || (pu.Host != "" && pu.Host != "localhost") {
			return URL{}, invalidURL("file URLs must not have a host")
		}
		if !strings.HasPrefix(pu.Path, "/") {
			return URL{}, invalidURL("file URL path must be absolute")
		}
		return u, nil
	}

	host := pu.Hostname()
	if host == "" {
		return URL{}, invalidURL("repository URL has no host")
	}
	if net.ParseIP(host) == nil && !hostRe.MatchString(host) {
		return URL{}, invalidURL("repository URL has an invalid host")
	}
	u.Host = pu.Host
	if pu.Path == "" || pu.Path == "/" {
		return URL{}, invalidURL("repository URL has no path")
	}

	switch scheme {
	case "https", "http":
		if pu.User != nil {
			return URL{}, invalidURL("credentials in the URL are not allowed; put tokens in the token parameter")
		}
		if scheme == "http" && !isLoopback(host) {
			return URL{}, invalidURL("plain http is only allowed for loopback hosts; use https")
		}
	case "ssh":
		if pu.User != nil {
			if _, hasPw := pu.User.Password(); hasPw || !userRe.MatchString(pu.User.Username()) {
				return URL{}, invalidURL("invalid user in ssh URL")
			}
		}
	}
	return u, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
