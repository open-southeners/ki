package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/open-southeners/ki/internal/edge/certs"
	"github.com/open-southeners/ki/internal/store"
)

// Exposure limits.
const (
	maxDomainsPerService = 20
	maxPathLen           = 256
)

var (
	serviceNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
)

// Exposure is the wire view of one exposed service.
type Exposure struct {
	Service string   `json:"service"`
	Port    int      `json:"port"`
	Domains []string `json:"domains"`
	Path    string   `json:"path,omitempty"`
	// Source is "labels" or "project".
	Source string `json:"source"`
}

// ExposeParams are the arguments of projects.expose.
type ExposeParams struct {
	Slug    string   `json:"slug"`
	Service string   `json:"service"`
	Port    int      `json:"port"`
	Domains []string `json:"domains"`
	Path    string   `json:"path"`
}

// ValidateExposure checks an exposure and returns its domains normalized
// (trimmed, lowercased, duplicates removed). Domains must be DNS names in
// IDNA ASCII form with at least one dot; wildcards and IP addresses are
// rejected. Errors are *InvalidError.
func ValidateExposure(service string, port int, domains []string, path string) ([]string, error) {
	if !serviceNameRe.MatchString(service) || len(service) > 128 {
		return nil, invalidf("invalid service name %q", service)
	}
	if port < 1 || port > 65535 {
		return nil, invalidf("port must be between 1 and 65535")
	}
	if len(domains) == 0 {
		return nil, invalidf("at least one domain is required")
	}
	if len(domains) > maxDomainsPerService {
		return nil, invalidf("at most %d domains per service", maxDomainsPerService)
	}
	out := make([]string, 0, len(domains))
	seen := map[string]bool{}
	for _, d := range domains {
		d = strings.ToLower(strings.TrimSpace(d))
		if err := validDomain(d); err != nil {
			return nil, err
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	if path != "" {
		switch {
		case !strings.HasPrefix(path, "/"):
			return nil, invalidf("path must start with /")
		case len(path) > maxPathLen:
			return nil, invalidf("path is longer than %d characters", maxPathLen)
		case strings.ContainsAny(path, " \t\r\n") || strings.Contains(path, ".."):
			return nil, invalidf("path must not contain spaces or ..")
		}
		for _, r := range path {
			if r < 0x21 || r == 0x7f {
				return nil, invalidf("path must not contain control characters")
			}
		}
	}
	return out, nil
}

func validDomain(d string) error {
	if err := certs.ValidHostname(d); err != nil {
		return invalidf("invalid domain %q: %s", d, err)
	}
	return nil
}

// ValidateExposure is the package function plus the server's reserved
// hostnames: exposing one of those is rejected.
func (s *Service) ValidateExposure(service string, port int, domains []string, path string) ([]string, error) {
	out, err := ValidateExposure(service, port, domains, path)
	if err != nil {
		return nil, err
	}
	s.hookMu.Lock()
	reserved := s.reserved
	s.hookMu.Unlock()
	if reserved != nil {
		held := map[string]bool{}
		for _, h := range reserved() {
			held[strings.ToLower(strings.TrimSpace(h))] = true
		}
		for _, d := range out {
			if held[d] {
				return nil, invalidf("domain %q is reserved for the server", d)
			}
		}
	}
	return out, nil
}

// SetReservedHostnames sets the func returning the server's own hostnames.
func (s *Service) SetReservedHostnames(fn func() []string) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.reserved = fn
}

// OnExposureChange registers fn to run after every successful Expose or
// Unexpose, outside the project lock.
func (s *Service) OnExposureChange(fn func()) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.onExposure = append(s.onExposure, fn)
}

func (s *Service) fireExposureChange() {
	s.hookMu.Lock()
	fns := append([]func(){}, s.onExposure...)
	s.hookMu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

// deployedServices returns the service names of the last deployed model, or
// nil when no deploy has produced one.
func (s *Service) deployedServices(slug string) map[string]bool {
	b, err := os.ReadFile(filepath.Join(s.dataDir, "deploys", slug, "compose.json"))
	if err != nil {
		return nil
	}
	var m struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if json.Unmarshal(b, &m) != nil || m.Services == nil {
		return nil
	}
	out := make(map[string]bool, len(m.Services))
	for name := range m.Services {
		out[name] = true
	}
	return out
}

// Expose sets the project-side exposure of a service (overriding any labels)
// and returns the updated project. A domain already routed elsewhere is a
// *store.DomainConflictError.
func (s *Service) Expose(ctx context.Context, in ExposeParams) (*Project, error) {
	domains, err := s.ValidateExposure(in.Service, in.Port, in.Domains, in.Path)
	if err != nil {
		return nil, err
	}
	v, err := s.expose(ctx, in, domains)
	if err != nil {
		return nil, err
	}
	s.fireExposureChange()
	return v, nil
}

func (s *Service) expose(ctx context.Context, in ExposeParams, domains []string) (*Project, error) {
	unlock := s.lock(in.Slug)
	defer unlock()
	p, err := s.st.GetProject(ctx, in.Slug)
	if err != nil {
		return nil, err
	}
	if known := s.deployedServices(in.Slug); known != nil && !known[in.Service] {
		return nil, invalidf("service %q does not exist in the deployed compose model", in.Service)
	}
	e := store.Exposure{Service: in.Service, Port: in.Port, Domains: domains, Path: in.Path}
	if err := s.st.SetProjectExposure(ctx, in.Slug, e); err != nil {
		return nil, err
	}
	s.log.Info("service exposed", "slug", in.Slug, "service", in.Service, "domains", domains)
	v := view(p)
	s.decorate(ctx, &v)
	return &v, nil
}

// Unexpose removes the project-side exposure of a service (labels, if any,
// apply again) and returns the updated project. It is a no-op when there is no
// project-side entry.
func (s *Service) Unexpose(ctx context.Context, slug, service string) (*Project, error) {
	v, err := s.unexpose(ctx, slug, service)
	if err != nil {
		return nil, err
	}
	s.fireExposureChange()
	return v, nil
}

func (s *Service) unexpose(ctx context.Context, slug, service string) (*Project, error) {
	if service == "" {
		return nil, invalidf("service is required")
	}
	unlock := s.lock(slug)
	defer unlock()
	p, err := s.st.GetProject(ctx, slug)
	if err != nil {
		return nil, err
	}
	if err := s.st.RemoveProjectExposure(ctx, slug, service); err != nil {
		return nil, err
	}
	s.log.Info("service unexposed", "slug", slug, "service", service)
	v := view(p)
	s.decorate(ctx, &v)
	return &v, nil
}

// decorate fills the exposure fields of v. Failures are logged and leave the
// fields empty: exposure is advisory on reads.
func (s *Service) decorate(ctx context.Context, v *Project) {
	if v.Source == SourceCompose {
		if n, err := s.st.LatestComposeVersion(ctx, v.Slug); err == nil {
			v.ComposeVersion = n
		}
	}
	es, err := s.st.ListExposures(ctx, v.Slug)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Warn("list exposures", "slug", v.Slug, "err", err)
		}
		return
	}
	if len(es) == 0 {
		return
	}
	known := s.deployedServices(v.Slug)
	v.Exposure = make([]Exposure, 0, len(es))
	for _, e := range es {
		domains := e.Domains
		if domains == nil {
			domains = []string{}
		}
		v.Exposure = append(v.Exposure, Exposure{Service: e.Service, Port: e.Port, Domains: domains, Path: e.Path, Source: e.Source})
		if e.Source == store.ExposureSourceProject && known == nil {
			v.ExposureWarnings = append(v.ExposureWarnings,
				fmt.Sprintf("service %q is not verified yet: the project has not been deployed", e.Service))
		} else if known != nil && !known[e.Service] {
			v.ExposureWarnings = append(v.ExposureWarnings,
				fmt.Sprintf("service %q does not exist in the deployed compose model", e.Service))
		}
		if e.Source != store.ExposureSourceLabels {
			continue
		}
		for _, d := range e.Domains {
			owner, svc, err := s.st.DomainOwner(ctx, d)
			if err == nil && (owner != v.Slug || svc != e.Service) {
				v.ExposureWarnings = append(v.ExposureWarnings,
					fmt.Sprintf("domain %q of service %q is not routed: it is exposed by %s/%s", d, e.Service, owner, svc))
			}
		}
	}
}
