package deploys

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/open-southeners/ki-agent/internal/compose"
	"github.com/open-southeners/ki-agent/internal/git"
	"github.com/open-southeners/ki-agent/internal/policy"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/store"
)

// maxDetail is the longest error detail kept on a deploy.
const maxDetail = 300

// stepError is a pipeline failure. The zero value (no reason) stands for a
// cancelled run.
type stepError struct {
	reason, detail, data string
}

func fail(reason, format string, args ...any) *stepError {
	return &stepError{reason: reason, detail: oneLine(fmt.Sprintf(format, args...))}
}

// oneLine collapses s to its last non-empty line, bounded in length.
func oneLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(lines[len(lines)-1])
	if len(s) > maxDetail {
		s = strings.ToValidUTF8(s[:maxDetail], "") + "..."
	}
	return s
}

// composeDetail is the one-line cause of a failed compose command.
func composeDetail(err error) string {
	var ce *compose.Error
	if errors.As(err, &ce) {
		if strings.TrimSpace(ce.Stderr) != "" {
			return oneLine(ce.Stderr)
		}
		return oneLine(ce.Err.Error())
	}
	return oneLine(err.Error())
}

// pipeline runs the whole deploy holding the project lock. It returns nil on
// success.
func (m *Manager) pipeline(j *job) *stepError {
	if j.ctx.Err() != nil {
		return &stepError{}
	}
	var se *stepError
	err := m.opts.Projects.WithProject(j.ctx, j.d.Project, func(p *store.Project, dir string) error {
		se = m.run(j, p, dir)
		return nil
	})
	switch {
	case se != nil:
		return se
	case errors.Is(err, store.ErrNotFound):
		return fail(ReasonInternal, "project not found")
	case err != nil:
		m.log.Error("deploy", "deploy", j.d.ID, "err", err)
		return fail(ReasonInternal, "%v", err)
	}
	return nil
}

func (m *Manager) agent(j *job, format string, args ...any) {
	j.bc.line("agent", fmt.Sprintf(format, args...))
}

func (m *Manager) stage(j *job, s string) {
	j.d.Stage = s
	m.save(j)
}

// run executes the stages. The caller holds the project lock.
func (m *Manager) run(j *job, p *store.Project, dir string) *stepError {
	ctx, slug := j.ctx, p.Slug
	out := j.bc.line

	// fetch
	m.stage(j, StageFetch)
	m.agent(j, "fetching %s", j.t.ref)
	auth, err := m.opts.Projects.Auth(p)
	if err != nil {
		m.log.Error("deploy credentials", "deploy", j.d.ID, "err", err)
		return fail(ReasonInternal, "cannot read repository credentials")
	}
	sha, err := m.opts.Git.Checkout(ctx, p.RepoURL, auth, dir, j.t.fetchRef, j.t.pin)
	if err != nil {
		var ge *git.Error
		if errors.As(err, &ge) {
			return fail(ReasonFetchFailed, "%s: %s", ge.Reason, ge.Detail)
		}
		m.log.Error("deploy checkout", "deploy", j.d.ID, "err", err)
		return fail(ReasonInternal, "checkout failed")
	}
	j.d.Commit = sha
	m.save(j)
	m.agent(j, "checked out %s", sha)
	if se := m.writeDotenv(ctx, slug, dir); se != nil {
		return se
	}

	// config
	m.stage(j, StageConfig)
	file := p.ComposeFile
	m.agent(j, "normalizing %s", file)
	if _, err := os.Stat(filepath.Join(dir, file)); errors.Is(err, fs.ErrNotExist) {
		return fail(ReasonComposeNotFound, "%s does not exist at %s", file, shortSHA(sha))
	}
	if err := compose.CheckIncludes(dir, file); err != nil {
		var ie *compose.IncludeError
		switch {
		case errors.As(err, &ie):
			return fail(ReasonComposeInvalid, "%v", ie)
		case errors.Is(err, fs.ErrNotExist):
			return fail(ReasonComposeNotFound, "%s does not exist at %s", file, shortSHA(sha))
		}
		m.log.Error("deploy include scan", "deploy", j.d.ID, "err", err)
		return fail(ReasonInternal, "cannot scan compose file")
	}
	model, err := m.opts.Compose.Config(ctx, dir, file, slug)
	if err != nil {
		var ce *compose.Error
		if errors.As(err, &ce) {
			for line := range strings.SplitSeq(strings.TrimSpace(ce.Stderr), "\n") {
				if line != "" {
					m.agent(j, "%s", line)
				}
			}
		}
		return fail(ReasonComposeInvalid, "%s", composeDetail(err))
	}

	// policy
	m.stage(j, StagePolicy)
	m.agent(j, "checking policy")
	proj, err := policy.ParseCompose(model)
	if err != nil {
		return fail(ReasonComposeInvalid, "%v", err)
	}
	if vs := policy.Check(dir, proj, policy.Options{}); len(vs) > 0 {
		for _, v := range vs {
			m.agent(j, "policy violation: %s %s: %s", v.Service, v.Rule, v.Detail)
		}
		data, _ := json.Marshal(map[string]any{"violations": vs})
		return &stepError{
			reason: ReasonPolicyViolation,
			detail: fmt.Sprintf("%d policy violation(s)", len(vs)),
			data:   string(data),
		}
	}
	exposures, se := m.exposures(j, slug, model)
	if se != nil {
		return se
	}
	labelled, err := compose.Label(model, slug)
	if err != nil {
		return fail(ReasonComposeInvalid, "%v", err)
	}
	modelFile, err := m.writeModel(slug, labelled)
	if err != nil {
		m.log.Error("deploy write model", "deploy", j.d.ID, "err", err)
		return fail(ReasonInternal, "cannot write the compose model")
	}

	// build
	m.stage(j, StageBuild)
	m.agent(j, "building")
	if err := m.opts.Compose.Build(ctx, modelFile, slug, out); err != nil {
		return fail(ReasonBuildFailed, "%s", composeDetail(err))
	}

	// up
	m.stage(j, StageUp)
	m.agent(j, "starting services")
	if err := m.opts.Compose.Up(ctx, modelFile, slug, out); err != nil {
		return fail(ReasonUpFailed, "%s", composeDetail(err))
	}

	// health
	m.stage(j, StageHealth)
	services := make([]string, 0, len(proj.Services))
	for name := range proj.Services {
		services = append(services, name)
	}
	sort.Strings(services)
	m.agent(j, "waiting for %d service(s) to be healthy", len(services))
	if se := m.waitHealthy(j, services); se != nil {
		return se
	}
	if err := m.opts.Projects.SetCommitLocked(ctx, p, sha); err != nil {
		m.log.Error("deploy record commit", "deploy", j.d.ID, "err", err)
		return fail(ReasonInternal, "cannot record the deployed commit")
	}
	// The domains were free before up; another project may have taken one
	// since. The deploy itself succeeded, so this is only a warning.
	if err := m.opts.Store.ReplaceLabelExposures(ctx, slug, exposures); err != nil {
		var dc *store.DomainConflictError
		if errors.As(err, &dc) {
			m.agent(j, "warning: exposure not updated: domain %s is already exposed by %s/%s", dc.Domain, dc.Project, dc.Service)
		} else {
			m.log.Error("deploy store exposures", "deploy", j.d.ID, "err", err)
			m.agent(j, "warning: exposure not updated: %v", err)
		}
	}
	return nil
}

// exposures reads and validates the ki.expose.* labels of the model and checks
// that their domains are not routed to another project (or twice within this
// model). Services the project has overridden through projects.expose are
// skipped: their label domains are not registered.
func (m *Manager) exposures(j *job, slug string, model []byte) ([]store.Exposure, *stepError) {
	ctx := j.ctx
	labels, err := compose.Exposures(model)
	if err != nil {
		return nil, fail(ReasonComposeInvalid, "%v", err)
	}
	out := make([]store.Exposure, 0, len(labels))
	for _, l := range labels {
		domains, err := m.opts.Projects.ValidateExposure(l.Service, l.Port, l.Domains, l.Path)
		if err != nil {
			return nil, fail(ReasonComposeInvalid, "service %q: %v", l.Service, err)
		}
		out = append(out, store.Exposure{Service: l.Service, Port: l.Port, Domains: domains, Path: l.Path})
	}
	if len(out) == 0 {
		return out, nil
	}
	m.agent(j, "checking %d exposed service(s)", len(out))
	effective, err := m.opts.Store.ListExposures(ctx, slug)
	if err != nil {
		m.log.Error("deploy list exposures", "deploy", j.d.ID, "err", err)
		return nil, fail(ReasonInternal, "cannot read the project exposure")
	}
	overridden := map[string]bool{}
	for _, e := range effective {
		if e.Source == store.ExposureSourceProject {
			overridden[e.Service] = true
		}
	}
	claimed := map[string]string{} // domain -> service of this model
	for _, e := range out {
		if overridden[e.Service] {
			continue
		}
		for _, d := range e.Domains {
			if other, ok := claimed[d]; ok && other != e.Service {
				return nil, fail(ReasonDomainConflict, "domain %s is used by services %s and %s of this project", d, other, e.Service)
			}
			claimed[d] = e.Service
			owner, svc, err := m.opts.Store.DomainOwner(ctx, d)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				m.log.Error("deploy domain owner", "deploy", j.d.ID, "err", err)
				return nil, fail(ReasonInternal, "cannot check domain %s", d)
			case owner != slug:
				return nil, fail(ReasonDomainConflict, "domain %s is already exposed by project %s", d, owner)
			case svc != e.Service && overridden[svc]:
				// Label entries of this project are replaced by this deploy, so
				// only a project-side entry of another service holds a domain.
				return nil, fail(ReasonDomainConflict, "domain %s is already exposed by service %s of this project", d, svc)
			}
		}
	}
	return out, nil
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// writeDotenv writes the project's variables to <dir>/.env, or removes the
// file when there are none.
func (m *Manager) writeDotenv(ctx context.Context, slug, dir string) *stepError {
	vars, err := m.opts.Projects.Env(ctx, slug)
	if err != nil {
		m.log.Error("deploy env", "slug", slug, "err", err)
		return fail(ReasonInternal, "cannot read the project environment")
	}
	path := filepath.Join(dir, ".env")
	// A symlink (or other special file) could make compose read files outside
	// the checkout, such as another project's secrets.
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fail(ReasonComposeInvalid, "'.env' must be a regular file")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Error("deploy stat .env", "slug", slug, "err", err)
		return fail(ReasonInternal, "cannot inspect .env")
	}
	if len(vars) == 0 {
		// A tracked .env belongs to the repository (checkout restored it).
		tracked, err := m.opts.Git.IsTracked(ctx, dir, ".env")
		if err != nil {
			m.log.Error("deploy ls-files", "slug", slug, "err", err)
			return fail(ReasonInternal, "cannot inspect the checkout")
		}
		if tracked {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			m.log.Error("deploy remove .env", "slug", slug, "err", err)
			return fail(ReasonInternal, "cannot remove the stale .env")
		}
		return nil
	}
	if err := compose.WriteDotenv(path, vars); err != nil {
		m.log.Error("deploy write .env", "slug", slug, "err", err)
		return fail(ReasonInternal, "cannot write .env")
	}
	return nil
}

// writeModel stores the labelled model, which is all that later stages run.
func (m *Manager) writeModel(slug string, model []byte) (string, error) {
	dir := filepath.Join(m.opts.DataDir, "deploys", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".compose-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.Write(model); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "compose.json")
	return path, os.Rename(tmp.Name(), path)
}

// waitHealthy polls the project's containers until every service is up.
func (m *Manager) waitHealthy(j *job, services []string) *stepError {
	hctx, cancel := context.WithTimeout(j.ctx, m.opts.HealthTimeout)
	defer cancel()
	tick := time.NewTicker(m.opts.PollInterval)
	defer tick.Stop()
	var last string
	for {
		waiting, fatal, err := m.checkHealth(hctx, j.d.Project, services)
		switch {
		case err != nil && j.ctx.Err() != nil:
			return &stepError{}
		case err != nil && errors.Is(err, runtime.ErrUnavailable):
			return fail(ReasonRuntimeUnavailable, "container runtime unavailable")
		case err != nil && hctx.Err() == nil:
			m.log.Error("deploy health", "deploy", j.d.ID, "err", err)
			return fail(ReasonInternal, "health check failed")
		case fatal != "":
			return fail(ReasonHealthFailed, "%s", fatal)
		case err == nil && waiting == "":
			m.agent(j, "all services are up")
			return nil
		}
		if err == nil && waiting != last {
			m.agent(j, "waiting: %s", waiting)
			last = waiting
		}
		select {
		case <-hctx.Done():
			if j.ctx.Err() != nil {
				return &stepError{}
			}
			if last == "" {
				last = "no containers"
			}
			return fail(ReasonHealthFailed, "timed out after %s waiting for: %s", m.opts.HealthTimeout, last)
		case <-tick.C:
		}
	}
}

// checkHealth reports what the deploy still waits for ("" when all services
// are up) or a fatal condition.
func (m *Manager) checkHealth(ctx context.Context, slug string, services []string) (waiting, fatal string, err error) {
	cs, err := m.opts.Runtime.List(ctx, slug)
	if err != nil {
		return "", "", err
	}
	byService := map[string][]runtime.Container{}
	for _, c := range cs {
		byService[c.Service] = append(byService[c.Service], c)
	}
	var wait []string
	for _, svc := range services {
		list := byService[svc]
		if len(list) == 0 {
			wait = append(wait, svc+" (no container yet)")
			continue
		}
		for _, c := range list {
			d, err := m.opts.Runtime.Inspect(ctx, c.ID)
			if errors.Is(err, runtime.ErrNotFound) {
				wait = append(wait, svc+" (container vanished)")
				continue
			}
			if err != nil {
				return "", "", err
			}
			switch {
			case d.State == "running" && d.Health == "unhealthy":
				return "", fmt.Sprintf("%s is unhealthy", svc), nil
			case d.State == "running" && d.Health == "starting":
				wait = append(wait, svc+" (health check starting)")
			case d.State == "running":
			case d.State == "exited" && d.ExitCode == 0:
				// A completed one-shot service.
			case d.State == "exited" || d.State == "dead":
				return "", fmt.Sprintf("%s exited with code %d", svc, d.ExitCode), nil
			default:
				wait = append(wait, fmt.Sprintf("%s (%s)", svc, d.State))
			}
		}
	}
	sort.Strings(wait)
	return strings.Join(wait, ", "), "", nil
}
