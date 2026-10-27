package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/open-southeners/ki/internal/compose"
)

// Image project limits.
const (
	maxImageVolumes = 20
	maxImageEnv     = 100
	maxEnvValueLen  = 4096
	imageNote       = "generated from image"
)

var (
	imageServiceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	volumeNameRe   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	volumePathRe   = regexp.MustCompile(`^/[A-Za-z0-9_./-]*$`)
	envKeyRe       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// DefaultImageService is the service name of a generated compose file when
// the spec names none.
const DefaultImageService = "app"

// ImageSpec describes a single-container project: the inputs of
// projects.create with image.
type ImageSpec struct {
	// Image is the full pinned reference (tag or digest required).
	Image   string   `json:"image"`
	Service string   `json:"service,omitempty"`
	Port    int      `json:"port,omitempty"`
	Domains []string `json:"domains,omitempty"`
	Path    string   `json:"path,omitempty"`
	// Volumes are named volumes: name:/container/path[:ro].
	Volumes []string `json:"volumes,omitempty"`
	// Env are plain, non-secret variables.
	Env map[string]string `json:"env,omitempty"`
}

// yamlQuote renders s as a double-quoted YAML scalar (JSON strings are valid
// YAML).
func yamlQuote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// GenerateCompose renders the compose file of an image spec. The output is
// deterministic. Errors are *InvalidError.
func GenerateCompose(spec ImageSpec) (string, error) {
	ref, err := compose.ParsePinnedImageRef(spec.Image)
	if err != nil {
		return "", invalidf("image: %v", err)
	}
	svc := spec.Service
	if svc == "" {
		svc = DefaultImageService
	}
	if !imageServiceRe.MatchString(svc) {
		return "", invalidf("service must match %s", imageServiceRe)
	}
	var domains []string
	switch {
	case spec.Port != 0 && (spec.Port < 1 || spec.Port > 65535):
		return "", invalidf("port must be between 1 and 65535")
	case len(spec.Domains) > 0 && spec.Port == 0:
		return "", invalidf("domains require port")
	case spec.Path != "" && len(spec.Domains) == 0:
		return "", invalidf("path requires domains")
	case len(spec.Domains) > 0:
		if domains, err = ValidateExposure(svc, spec.Port, spec.Domains, spec.Path); err != nil {
			return "", err
		}
	}

	type vol struct {
		name, path string
		ro         bool
	}
	var vols []vol
	if len(spec.Volumes) > maxImageVolumes {
		return "", invalidf("at most %d volumes", maxImageVolumes)
	}
	targets := map[string]bool{}
	for _, v := range spec.Volumes {
		name, rest, ok := strings.Cut(v, ":")
		if !ok || !volumeNameRe.MatchString(name) {
			return "", invalidf("invalid volume %q: want name:/container/path[:ro] with a named volume (bind mounts are not allowed)", v)
		}
		path, mode, hasMode := strings.Cut(rest, ":")
		if hasMode && mode != "ro" {
			return "", invalidf("invalid volume %q: the only option is ro", v)
		}
		if path == "/" || !volumePathRe.MatchString(path) || strings.Contains(path, "..") || strings.Contains(path, "//") {
			return "", invalidf("invalid volume %q: container path must be absolute", v)
		}
		if targets[path] {
			return "", invalidf("volume path %q is mounted twice", path)
		}
		targets[path] = true
		vols = append(vols, vol{name, path, hasMode})
	}

	if len(spec.Env) > maxImageEnv {
		return "", invalidf("at most %d env variables", maxImageEnv)
	}
	keys := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		switch {
		case !envKeyRe.MatchString(k):
			return "", invalidf("invalid env name %q", k)
		case strings.ContainsAny(v, "\r\n\x00"):
			return "", invalidf("env %s: value must be a single line", k)
		case len(v) > maxEnvValueLen:
			return "", invalidf("env %s: value is longer than %d characters", k, maxEnvValueLen)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	b.WriteString("services:\n")
	fmt.Fprintf(&b, "  %s:\n", svc)
	fmt.Fprintf(&b, "    image: %s\n", ref)
	b.WriteString("    restart: unless-stopped\n")
	b.WriteString("    env_file:\n      - path: .env\n        required: false\n")
	if len(keys) > 0 {
		b.WriteString("    environment:\n")
		for _, k := range keys {
			// $ is compose interpolation: keep values literal.
			fmt.Fprintf(&b, "      %s: %s\n", k, yamlQuote(strings.ReplaceAll(spec.Env[k], "$", "$$")))
		}
	}
	if spec.Port != 0 {
		fmt.Fprintf(&b, "    expose:\n      - %s\n", yamlQuote(fmt.Sprint(spec.Port)))
	}
	if len(vols) > 0 {
		b.WriteString("    volumes:\n")
		for _, v := range vols {
			s := v.name + ":" + v.path
			if v.ro {
				s += ":ro"
			}
			fmt.Fprintf(&b, "      - %s\n", s)
		}
	}
	if len(domains) > 0 {
		b.WriteString("    labels:\n")
		fmt.Fprintf(&b, "      %s: %s\n", compose.LabelExposePort, yamlQuote(fmt.Sprint(spec.Port)))
		fmt.Fprintf(&b, "      %s: %s\n", compose.LabelExposeDomains, yamlQuote(strings.Join(domains, ",")))
		if spec.Path != "" {
			fmt.Fprintf(&b, "      %s: %s\n", compose.LabelExposePath, yamlQuote(spec.Path))
		}
	}
	if len(vols) > 0 {
		names := map[string]bool{}
		for _, v := range vols {
			names[v.name] = true
		}
		sorted := make([]string, 0, len(names))
		for n := range names {
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		b.WriteString("volumes:\n")
		for _, n := range sorted {
			fmt.Fprintf(&b, "  %s:\n", n)
		}
	}
	return b.String(), nil
}

// createImage creates a compose project from an image spec: the generated
// file goes through the normal compose create path.
func (s *Service) createImage(ctx context.Context, in CreateParams) (*Result, error) {
	if in.Slug == "" {
		return nil, invalidf("slug is required for image projects")
	}
	if len(in.Image.Domains) > 0 && in.Image.Port != 0 {
		svc := in.Image.Service
		if svc == "" {
			svc = DefaultImageService
		}
		if _, err := s.ValidateExposure(svc, in.Image.Port, in.Image.Domains, in.Image.Path); err != nil {
			return nil, err
		}
	}
	content, err := GenerateCompose(*in.Image)
	if err != nil {
		return nil, err
	}
	in.Compose, in.Image, in.note = content, nil, imageNote
	return s.createCompose(ctx, in)
}

// ImageSetParams are the arguments of projects.image.set.
type ImageSetParams struct {
	Slug string `json:"slug"`
	// Image is the full new reference; Tag replaces only the tag or digest
	// of the service's current one. Exactly one is set.
	Image   string `json:"image"`
	Tag     string `json:"tag"`
	Service string `json:"service"`
	Note    string `json:"note"`
	Deploy  bool   `json:"deploy"`
	// CreatedBy is filled in by the handler.
	CreatedBy string `json:"-"`
}

// ImageSetResult is the result of projects.image.set.
type ImageSetResult struct {
	Version int    `json:"version"`
	Image   string `json:"image"`
	Deploy  any    `json:"deploy,omitempty"`
}

// mapValue returns the value node under key in a mapping node, or nil.
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// ImageSet sets the image of one service of a compose project's latest
// version, storing the result as a new version (optionally deploying it).
func (s *Service) ImageSet(ctx context.Context, in ImageSetParams) (*ImageSetResult, error) {
	switch {
	case in.Image != "" && in.Tag != "":
		return nil, invalidf("image and tag are mutually exclusive")
	case in.Image == "" && in.Tag == "":
		return nil, invalidf("image or tag is required")
	}
	if err := s.composeProject(ctx, in.Slug); err != nil {
		return nil, err
	}
	cv, err := s.st.GetComposeVersion(ctx, in.Slug, 0)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(cv.Content), &doc); err != nil || len(doc.Content) == 0 {
		return nil, invalidf("current compose file is not valid YAML")
	}
	services := mapValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, invalidf("compose file has no services")
	}
	var withImage, all []string
	for i := 0; i+1 < len(services.Content); i += 2 {
		name := services.Content[i].Value
		all = append(all, name)
		if mapValue(services.Content[i+1], "image") != nil {
			withImage = append(withImage, name)
		}
	}
	svc := in.Service
	switch {
	case svc == "" && len(withImage) == 1:
		svc = withImage[0]
	case svc == "" && len(withImage) == 0:
		return nil, invalidf("no service has an image (services: %s)", strings.Join(all, ", "))
	case svc == "":
		return nil, invalidf("several services have an image; pass service (one of: %s)", strings.Join(withImage, ", "))
	}
	var node *yaml.Node
	for i := 0; i+1 < len(services.Content); i += 2 {
		if services.Content[i].Value == svc {
			node = mapValue(services.Content[i+1], "image")
		}
	}
	if node == nil || node.Kind != yaml.ScalarNode {
		return nil, invalidf("service %q has no image (services with an image: %s)", svc, strings.Join(withImage, ", "))
	}

	var ref compose.ImageRef
	if in.Image != "" {
		ref, err = compose.ParsePinnedImageRef(in.Image)
	} else {
		var cur compose.ImageRef
		if cur, err = compose.ParseImageRef(node.Value); err != nil {
			return nil, invalidf("current image %q of service %q cannot be parsed: %v", node.Value, svc, err)
		}
		ref, err = cur.WithVersion(in.Tag)
	}
	if err != nil {
		return nil, invalidf("image: %v", err)
	}
	node.Value, node.Tag, node.Style = ref.String(), "!!str", 0

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	note := in.Note
	if note == "" {
		note = "image " + ref.String()
	}
	res, err := s.ComposeSetAndDeploy(ctx, in.Slug, out.String(), note, in.CreatedBy, in.Deploy)
	if err != nil {
		return nil, err
	}
	return &ImageSetResult{Version: res.Version, Image: ref.String(), Deploy: res.Deploy}, nil
}
