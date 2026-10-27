package compose

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	imageTagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	imageDigestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	imageHostRe   = regexp.MustCompile(`^(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])(?:\.(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9]))*(?::[0-9]{1,5})?$`)
	imagePartRe   = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
)

// maxImageRefLen bounds a whole image reference.
const maxImageRefLen = 255

// ImageRef is a parsed image reference: [registry/]path[:tag][@digest].
type ImageRef struct {
	// Registry is the explicit registry host (with optional port), "" for a
	// Docker Hub short form such as nginx or acme/web.
	Registry string
	// Path is the repository path below the registry (lowercase).
	Path string
	// Tag and Digest are as written; at least one is set for a pinned ref.
	Tag    string
	Digest string
}

// Repo is the reference without tag and digest.
func (r ImageRef) Repo() string {
	if r.Registry == "" {
		return r.Path
	}
	return r.Registry + "/" + r.Path
}

// String is the reference as it should be written in a compose file.
func (r ImageRef) String() string {
	s := r.Repo()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + r.Digest
	}
	return s
}

// ParseImageRef parses and validates an image reference. It does not
// normalize: nginx:1.27 stays a Docker Hub short form.
func ParseImageRef(ref string) (ImageRef, error) {
	var out ImageRef
	if ref == "" {
		return out, errors.New("image is required")
	}
	if len(ref) > maxImageRefLen {
		return out, fmt.Errorf("image reference is longer than %d characters", maxImageRefLen)
	}
	name, digest, hasDigest := strings.Cut(ref, "@")
	if hasDigest {
		if !imageDigestRe.MatchString(digest) {
			return out, fmt.Errorf("invalid image digest %q: want sha256:<64 hex>", digest)
		}
		out.Digest = digest
	}
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		out.Tag = name[i+1:]
		name = name[:i]
		if !imageTagRe.MatchString(out.Tag) {
			return out, fmt.Errorf("invalid image tag %q", out.Tag)
		}
	}
	if name == "" {
		return out, errors.New("image name is empty")
	}
	parts := strings.Split(name, "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		if !imageHostRe.MatchString(parts[0]) {
			return out, fmt.Errorf("invalid registry %q", parts[0])
		}
		out.Registry = parts[0]
		parts = parts[1:]
	}
	for _, p := range parts {
		if !imagePartRe.MatchString(p) {
			return out, fmt.Errorf("invalid image name %q: repository path must be lowercase letters, digits and . _ - separators", name)
		}
	}
	out.Path = strings.Join(parts, "/")
	return out, nil
}

// ParsePinnedImageRef is ParseImageRef plus the pinning rule: a tag or digest
// is required and a bare latest tag is refused.
func ParsePinnedImageRef(ref string) (ImageRef, error) {
	r, err := ParseImageRef(ref)
	if err != nil {
		return r, err
	}
	if r.Digest == "" && (r.Tag == "" || r.Tag == "latest") {
		return r, errors.New("pin a tag or digest: untagged and latest images are not allowed")
	}
	return r, nil
}

// WithVersion returns r with its tag and digest replaced by v: a sha256:
// digest, or a tag. The result is pinned (latest is refused).
func (r ImageRef) WithVersion(v string) (ImageRef, error) {
	r.Tag, r.Digest = "", ""
	if strings.HasPrefix(v, "sha256:") {
		r.Digest = v
	} else {
		r.Tag = v
	}
	return ParsePinnedImageRef(r.String())
}
