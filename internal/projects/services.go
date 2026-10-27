package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/open-southeners/ki/internal/runtime"
)

// Service states.
const (
	ServiceRunning   = "running"
	ServicePartial   = "partial"
	ServiceStopped   = "stopped"
	ServiceUnhealthy = "unhealthy"
	// ServiceMissing means the service is in the deployed model but has no
	// container.
	ServiceMissing = "missing"
)

// serviceActionTimeout is the grace period of stop and restart.
const serviceActionTimeout = 10 * time.Second

// serviceActionBudget is added to the per-container grace period to bound a
// whole action.
const serviceActionBudget = 60 * time.Second

// composeServiceLabel is the label compose puts on its containers; it is the
// fallback when the ki.service label is absent.
const composeServiceLabel = "com.docker.compose.service"

// ErrServiceNotFound is returned for a service that is neither in the
// deployed model nor backed by a container.
var ErrServiceNotFound = errors.New("service not found")

// ServiceContainer is one container of a ProjectService.
type ServiceContainer struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	State        string     `json:"state"`
	Status       string     `json:"status"`
	Health       string     `json:"health"`
	RestartCount int        `json:"restart_count"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
}

// ProjectService is the wire view of one service of a project (the Service
// type is the projects service itself).
type ProjectService struct {
	Name       string             `json:"name"`
	Image      string             `json:"image,omitempty"`
	Exposure   *Exposure          `json:"exposure,omitempty"`
	Containers []ServiceContainer `json:"containers"`
	// State is one of the Service* constants.
	State string `json:"state"`
}

// deployedModel returns the services of the last deployed model with their
// images; ok is false when no deploy has produced a model.
func (s *Service) deployedModel(slug string) (images map[string]string, ok bool) {
	b, err := os.ReadFile(filepath.Join(s.dataDir, "deploys", slug, "compose.json"))
	if err != nil {
		return nil, false
	}
	var m struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal(b, &m) != nil || m.Services == nil {
		return nil, false
	}
	images = make(map[string]string, len(m.Services))
	for name, svc := range m.Services {
		images[name] = svc.Image
	}
	return images, true
}

func containerService(c runtime.Container) string {
	if c.Service != "" {
		return c.Service
	}
	return c.Labels[composeServiceLabel]
}

// Services lists the services of a project: those of the last deployed model
// plus any labelled container whose service is not in it.
func (s *Service) Services(ctx context.Context, slug string) ([]ProjectService, error) {
	if _, err := s.st.GetProject(ctx, slug); err != nil {
		return nil, err
	}
	return s.buildServices(ctx, slug, "")
}

// buildServices assembles the services; only (when set) restricts the result
// to one service and fails with ErrServiceNotFound when it does not exist.
func (s *Service) buildServices(ctx context.Context, slug, only string) ([]ProjectService, error) {
	model, haveModel := s.deployedModel(slug)
	cs, err := s.rt.List(ctx, slug)
	if err != nil {
		return nil, err
	}
	byName := map[string][]runtime.Container{}
	images := map[string]string{}
	for _, c := range cs {
		name := containerService(c)
		if name == "" || (only != "" && name != only) {
			continue
		}
		byName[name] = append(byName[name], c)
		if images[name] == "" {
			images[name] = c.Image
		}
	}
	names := map[string]bool{}
	for name := range byName {
		names[name] = true
	}
	if haveModel {
		for name := range model {
			if only == "" || name == only {
				names[name] = true
			}
		}
	}
	if only != "" && !names[only] {
		return nil, ErrServiceNotFound
	}
	exposures := map[string]Exposure{}
	if es, err := s.st.ListExposures(ctx, slug); err == nil {
		for _, e := range es {
			domains := e.Domains
			if domains == nil {
				domains = []string{}
			}
			exposures[e.Service] = Exposure{Service: e.Service, Port: e.Port, Domains: domains, Path: e.Path, Source: e.Source}
		}
	}

	out := make([]ProjectService, 0, len(names))
	for name := range names {
		ps := ProjectService{Name: name, Image: model[name], Containers: []ServiceContainer{}}
		if ps.Image == "" {
			ps.Image = images[name]
		}
		if e, ok := exposures[name]; ok {
			ps.Exposure = &e
		}
		cl := byName[name]
		sort.Slice(cl, func(i, j int) bool { return cl[i].Name < cl[j].Name })
		for _, c := range cl {
			d, err := s.rt.Inspect(ctx, c.ID)
			if errors.Is(err, runtime.ErrNotFound) {
				continue // vanished since List
			}
			if err != nil {
				return nil, err
			}
			sc := ServiceContainer{ID: c.ID, Name: c.Name, State: d.State, Status: d.Status,
				Health: d.Health, RestartCount: d.RestartCount}
			if sc.State == "" {
				sc.State, sc.Status = c.State, c.Status
			}
			if !d.StartedAt.IsZero() {
				t := d.StartedAt
				sc.StartedAt = &t
			}
			ps.Containers = append(ps.Containers, sc)
		}
		ps.State = deriveServiceState(ps.Containers)
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// deriveServiceState: missing without containers; unhealthy when any
// container is unhealthy or restarting; otherwise running when all run,
// stopped when none do, partial in between.
func deriveServiceState(cs []ServiceContainer) string {
	if len(cs) == 0 {
		return ServiceMissing
	}
	running := 0
	for _, c := range cs {
		if c.Health == "unhealthy" || c.State == "restarting" {
			return ServiceUnhealthy
		}
		if c.State == "running" {
			running++
		}
	}
	switch running {
	case 0:
		return ServiceStopped
	case len(cs):
		return ServiceRunning
	}
	return ServicePartial
}

// StartService starts every container of the service.
func (s *Service) StartService(ctx context.Context, slug, service string) (*ProjectService, error) {
	return s.serviceAction(ctx, slug, service, "start", func(ctx context.Context, id string) error {
		return s.rt.Start(ctx, id)
	})
}

// StopService stops every container of the service (10 s grace).
func (s *Service) StopService(ctx context.Context, slug, service string) (*ProjectService, error) {
	return s.serviceAction(ctx, slug, service, "stop", func(ctx context.Context, id string) error {
		return s.rt.Stop(ctx, id, serviceActionTimeout)
	})
}

// RestartService restarts every container of the service (10 s grace).
func (s *Service) RestartService(ctx context.Context, slug, service string) (*ProjectService, error) {
	return s.serviceAction(ctx, slug, service, "restart", func(ctx context.Context, id string) error {
		return s.rt.Restart(ctx, id, serviceActionTimeout)
	})
}

// serviceAction applies fn to every container of the service, attempting all
// of them and returning the first failure, then returns the updated service.
func (s *Service) serviceAction(ctx context.Context, slug, service, verb string, fn func(ctx context.Context, id string) error) (*ProjectService, error) {
	if service == "" {
		return nil, invalidf("service is required")
	}
	if _, err := s.st.GetProject(ctx, slug); err != nil {
		return nil, err
	}
	cur, err := s.buildServices(ctx, slug, service)
	if err != nil {
		return nil, err
	}
	// Once started, an action completes even if the caller goes away, so a
	// multi-container service is never left half-applied.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceActionBudget+time.Duration(len(cur[0].Containers))*serviceActionTimeout)
	defer cancel()
	var first error
	for _, c := range cur[0].Containers {
		err := fn(actx, c.ID)
		if errors.Is(err, runtime.ErrNotFound) {
			continue
		}
		if err != nil && first == nil {
			first = err
			if errors.Is(err, runtime.ErrUnavailable) {
				break
			}
		}
	}
	if first != nil {
		if errors.Is(first, runtime.ErrUnavailable) {
			return nil, first
		}
		return nil, fmt.Errorf("%s service %s/%s: %w", verb, slug, service, first)
	}
	s.log.Info("service "+verb, "slug", slug, "service", service, "containers", len(cur[0].Containers))
	after, err := s.buildServices(actx, slug, service)
	if err != nil {
		return nil, err
	}
	return &after[0], nil
}
