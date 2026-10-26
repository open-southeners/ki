package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/edge/certs"
	"github.com/open-southeners/ki-agent/internal/runtime"
	"github.com/open-southeners/ki-agent/internal/store"
)

// Edge states reported in [Status].
const (
	StateDisabled = "disabled"
	StateStarting = "starting"
	StateRunning  = "running"
	StateError    = "error"
)

// Certificate sources reported in [CertStatus].
const (
	SourceACME      = "acme"
	SourceInstalled = "installed"
	SourceImported  = "imported"
)

const (
	defaultInterval = 30 * time.Second
	minBackoff      = 30 * time.Second
	maxBackoff      = 5 * time.Minute
	maxErrorLen     = 300
)

// Composer starts the edge Compose project.
type Composer interface {
	Up(ctx context.Context, file, project string, out func(stream, line string)) error
}

// Runtime is the part of runtime.Runtime the manager uses.
type Runtime interface {
	List(ctx context.Context, project string) ([]runtime.Container, error)
	Inspect(ctx context.Context, id string) (runtime.ContainerDetail, error)
}

// Exposures lists the effective exposures of all projects.
type Exposures interface {
	AllExposures(ctx context.Context) ([]store.ProjectExposure, error)
}

// Options configure a [Manager].
type Options struct {
	Config config.Edge
	// DataDir is the agent data directory; the edge lives in <DataDir>/edge.
	DataDir   string
	Compose   Composer
	Runtime   Runtime
	Exposures Exposures
	Logger    *slog.Logger
	// APIEdgePort, PanelEnabled and RPCPublic feed the panel and RPC routes
	// ([Model]). The zero values add none, leaving only the webhook routes.
	APIEdgePort  int
	PanelEnabled bool
	RPCPublic    bool

	// interval and backoff bounds override the defaults in tests.
	interval, minBackoff, maxBackoff time.Duration
}

// SelfSignedStatus describes the default certificate.
type SelfSignedStatus struct {
	Fingerprint string    `json:"fingerprint_sha256"`
	NotAfter    time.Time `json:"not_after"`
}

// CertStatus describes a certificate the edge serves or will obtain.
type CertStatus struct {
	// ID identifies the certificate for edge.certificate.remove: the
	// hostname for acme and installed certificates, "i-<hex>" for imported.
	ID string `json:"id"`
	// Hostname is the first of Names (kept for older consumers).
	Hostname string   `json:"hostname"`
	Names    []string `json:"names"`
	// Source is "acme", "installed" or "imported".
	Source string `json:"source"`
	// Server reports whether the certificate serves server hostnames.
	Server   bool       `json:"server"`
	NotAfter *time.Time `json:"not_after,omitempty"`
}

func hostCert(host, source string, server bool, notAfter *time.Time) CertStatus {
	return CertStatus{ID: host, Hostname: host, Names: []string{host}, Source: source, Server: server, NotAfter: notAfter}
}

func importedCert(c certs.Imported) CertStatus {
	t := c.NotAfter
	return CertStatus{ID: c.ID, Hostname: c.Names[0], Names: c.Names, Source: SourceImported, Server: c.Server, NotAfter: &t}
}

// Status is the wire form of the edge state (EdgeStatus).
type Status struct {
	Enabled   bool   `json:"enabled"`
	State     string `json:"state"`
	LastError string `json:"last_error,omitempty"`
	Image     string `json:"image"`
	HTTPPort  int    `json:"http_port"`
	HTTPSPort int    `json:"https_port"`
	// Mode is "front" or "behind"; Listen is the address the proxy binds
	// in behind mode (edge.behind_listen) and empty in front mode.
	Mode            string           `json:"mode"`
	Listen          string           `json:"listen,omitempty"`
	ServerHostnames []string         `json:"server_hostnames"`
	SelfSigned      SelfSignedStatus `json:"self_signed"`
	Certificates    []CertStatus     `json:"certificates"`
	Routes          int              `json:"routes"`
}

// Manager keeps the edge proxy and its routing files in sync with the
// exposures and containers of the agent. It never makes the agent fail:
// problems are logged and reported through [Manager.Status], and retried with
// backoff.
type Manager struct {
	opt   Options
	log   *slog.Logger
	dir   string
	image string

	trigger chan struct{}

	mu         sync.Mutex
	state      string
	lastErr    string
	selfSigned certs.Cert
	certList   []CertStatus
	routes     int
	started    bool
	cancel     context.CancelFunc
	done       chan struct{}

	// Only touched by the reconcile goroutine (and Start before it runs).
	upHash string
}

// New returns a Manager; call [Manager.Start] to run it.
func New(o Options) *Manager {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.interval <= 0 {
		o.interval = defaultInterval
	}
	if o.minBackoff <= 0 {
		o.minBackoff = minBackoff
	}
	if o.maxBackoff <= 0 {
		o.maxBackoff = maxBackoff
	}
	image := o.Config.Image
	if image == "" {
		image = DefaultImage
	}
	m := &Manager{
		opt: o, log: o.Logger, image: image,
		dir:     filepath.Join(o.DataDir, "edge"),
		trigger: make(chan struct{}, 1),
		state:   StateDisabled,
	}
	if o.Config.Enabled {
		m.state = StateStarting
	}
	return m
}

// Start prepares the edge directory, the default certificate and the ACME
// storage, then runs the reconcile loop until [Manager.Close] or ctx is done.
// With the edge disabled it does nothing. Preparation failures do not make
// Start fail: they are reported in the status and retried by the loop. Start
// returns an error only when called twice.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return errors.New("edge: already started")
	}
	m.started = true
	m.mu.Unlock()
	if !m.opt.Config.Enabled {
		return nil
	}
	if err := m.prepare(); err != nil {
		m.log.Warn("edge: prepare failed", "err", err)
		m.setState(StateError, shorten(err))
	}
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	m.cancel, m.done = cancel, make(chan struct{})
	m.mu.Unlock()
	go m.loop(ctx)
	return nil
}

// Trigger asks for a reconcile soon. It never blocks and coalesces bursts.
func (m *Manager) Trigger() {
	select {
	case m.trigger <- struct{}{}:
	default:
	}
}

// Close stops the loop. The edge container keeps running (it survives agent
// restarts).
func (m *Manager) Close() error {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return nil
}

// Status returns a snapshot of the edge state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{
		Enabled: m.opt.Config.Enabled, State: m.state, LastError: m.lastErr,
		Image: m.image, HTTPPort: m.opt.Config.HTTPPort, HTTPSPort: m.opt.Config.HTTPSPort,
		SelfSigned: SelfSignedStatus{Fingerprint: m.selfSigned.FingerprintSHA256, NotAfter: m.selfSigned.NotAfter},
		Routes:     m.routes,
	}
	s.Mode = m.opt.Config.Mode
	if s.Mode == "" {
		s.Mode = config.EdgeModeFront
	}
	if s.Mode == config.EdgeModeBehind {
		s.Listen = m.opt.Config.BehindListen
	}
	// Installed certificates are read live so install/remove show at once;
	// the ACME entries come from the last reconcile.
	s.Certificates = []CertStatus{}
	live := map[string]bool{}
	if inst, err := certs.List(m.dir); err == nil {
		for _, c := range inst {
			t := c.NotAfter
			live[c.Hostname] = true
			s.Certificates = append(s.Certificates, hostCert(c.Hostname, SourceInstalled, true, &t))
		}
	}
	if imp, err := certs.ListImported(m.dir); err == nil {
		for _, c := range imp {
			if len(c.Names) > 0 {
				s.Certificates = append(s.Certificates, importedCert(c))
			}
		}
	}
	for _, c := range m.certList {
		if c.Source == SourceACME && !live[c.Hostname] {
			s.Certificates = append(s.Certificates, c)
		}
	}
	sort.Slice(s.Certificates, func(i, j int) bool {
		a, b := s.Certificates[i], s.Certificates[j]
		if a.Hostname != b.Hostname {
			return a.Hostname < b.Hostname
		}
		return a.ID < b.ID
	})
	s.ServerHostnames = m.ServerHostnames()
	return s
}

// ServerHostnames lists the hostnames the server itself answers on: the
// standalone edge.domain, the hostnames with an installed certificate and the
// non-wildcard names of imported certificates marked server,
// sorted. It is never nil.
func (m *Manager) ServerHostnames() []string {
	out := []string{}
	if d := m.opt.Config.Domain; d != "" {
		out = append(out, d)
	}
	if inst, err := certs.List(m.dir); err == nil {
		for _, c := range inst {
			out = append(out, c.Hostname)
		}
	}
	if imp, err := certs.ListImported(m.dir); err == nil {
		for _, c := range imp {
			if !c.Server {
				continue
			}
			for _, n := range c.Names {
				if !strings.HasPrefix(n, "*.") {
					out = append(out, n)
				}
			}
		}
	}
	sort.Strings(out)
	uniq := out[:0]
	for i, h := range out {
		if i == 0 || h != out[i-1] {
			uniq = append(uniq, h)
		}
	}
	return uniq
}

func (m *Manager) setState(state, errMsg string) {
	m.mu.Lock()
	m.state, m.lastErr = state, errMsg
	m.mu.Unlock()
}

func (m *Manager) loop(ctx context.Context) {
	defer close(m.done)
	backoff := time.Duration(0)
	wait := time.Duration(0) // reconcile immediately
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.trigger:
			// A trigger during a backoff retries right away.
		case <-timer.C:
		}
		timer.Stop()
		err := m.reconcile(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if backoff == 0 {
				backoff = m.opt.minBackoff
			} else if backoff *= 2; backoff > m.opt.maxBackoff {
				backoff = m.opt.maxBackoff
			}
			m.log.Warn("edge: reconcile failed", "err", err, "retry_in", backoff)
			m.setState(StateError, shorten(err))
			wait = backoff
			continue
		}
		backoff = 0
		m.setState(StateRunning, "")
		wait = m.opt.interval
	}
}

// prepare creates the directories and files the proxy mounts.
func (m *Manager) prepare() error {
	for _, d := range []struct {
		p    string
		mode os.FileMode
	}{{m.dir, 0o755}, {filepath.Join(m.dir, "dynamic"), 0o755}, {filepath.Join(m.dir, "certs"), 0o700}, {filepath.Join(m.dir, "acme"), 0o700}} {
		if err := os.MkdirAll(d.p, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.p, d.mode); err != nil {
			return err
		}
	}
	acme := filepath.Join(m.dir, "acme", "acme.json")
	f, err := os.OpenFile(acme, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(acme, 0o600); err != nil {
		return err
	}
	c, err := certs.LoadOrCreateSelfSigned(m.dir)
	if err != nil {
		return fmt.Errorf("self-signed certificate: %w", err)
	}
	m.mu.Lock()
	m.selfSigned = c
	m.mu.Unlock()
	return nil
}

// reconcile regenerates the files and (re)starts the proxy when needed.
func (m *Manager) reconcile(ctx context.Context) error {
	if err := m.prepare(); err != nil {
		return err
	}
	m.mu.Lock()
	self := m.selfSigned
	m.mu.Unlock()
	cfg := m.opt.Config

	installed, err := certs.List(m.dir)
	if err != nil {
		return fmt.Errorf("list certificates: %w", err)
	}
	imported, err := certs.ListImported(m.dir)
	if err != nil {
		return fmt.Errorf("list imported certificates: %w", err)
	}
	routes, err := m.projectRoutes(ctx)
	if err != nil {
		return err
	}
	model := Model{
		WebhooksPort: cfg.WebhooksPort, ACMEHostname: cfg.Domain,
		Behind:         cfg.Mode == config.EdgeModeBehind,
		SelfSignedCert: filepath.Base(self.CertFile), SelfSignedKey: filepath.Base(self.KeyFile),
		Routes:      routes,
		APIEdgePort: m.opt.APIEdgePort, PanelEnabled: m.opt.PanelEnabled, RPCPublic: m.opt.RPCPublic,
	}
	for _, c := range installed {
		model.Installed = append(model.Installed, InstalledCert{Hostname: c.Hostname})
	}
	var storedNames []string
	for _, c := range installed {
		storedNames = append(storedNames, c.Hostname)
	}
	for _, c := range imported {
		model.Imported = append(model.Imported, ImportedCert{ID: c.ID, Names: c.Names, Server: c.Server})
		storedNames = append(storedNames, c.Names...)
	}
	dyn, err := Dynamic(model)
	if err != nil {
		return err
	}
	if _, err := writeIfChanged(filepath.Join(m.dir, "dynamic", "routes.yaml"), dyn, 0o644); err != nil {
		return fmt.Errorf("write routes: %w", err)
	}

	static, err := Static(StaticOptions{
		HTTPPort: cfg.HTTPPort, HTTPSPort: cfg.HTTPSPort, ACMEEmail: cfg.ACMEEmail,
		Behind: model.Behind, BehindListen: cfg.BehindListen,
	})
	if err != nil {
		return err
	}
	if _, err := writeIfChanged(filepath.Join(m.dir, "traefik.yaml"), static, 0o644); err != nil {
		return fmt.Errorf("write static config: %w", err)
	}
	comp, err := Compose(ComposeOptions{
		Image: m.image, Dir: m.dir, ConfigHash: hashOf(static),
		SelfSignedCert: model.SelfSignedCert, SelfSignedKey: model.SelfSignedKey,
	})
	if err != nil {
		return err
	}
	composeFile := filepath.Join(m.dir, "compose.json")
	if _, err := writeIfChanged(composeFile, comp, 0o644); err != nil {
		return fmt.Errorf("write compose file: %w", err)
	}

	hash := hashOf(append(append([]byte{}, static...), comp...))
	running, err := m.edgeRunning(ctx)
	if err != nil {
		return err
	}
	if hash != m.upHash || !running {
		var tail []string
		err := m.opt.Compose.Up(ctx, composeFile, ProjectName, func(stream, line string) {
			if stream == "stderr" {
				if tail = append(tail, line); len(tail) > 5 {
					tail = tail[1:]
				}
			}
		})
		if err != nil {
			m.log.Warn("edge: compose up failed", "err", err)
			if len(tail) > 0 {
				return fmt.Errorf("start edge: %s", tail[len(tail)-1])
			}
			return fmt.Errorf("start edge: %w", err)
		}
		m.upHash = hash
		m.log.Info("edge: started", "image", m.image)
	}

	status := []CertStatus{}
	seen := map[string]bool{}
	for _, c := range installed {
		t := c.NotAfter
		status = append(status, hostCert(c.Hostname, SourceInstalled, true, &t))
		seen[c.Hostname] = true
	}
	for _, c := range imported {
		if len(c.Names) > 0 {
			status = append(status, importedCert(c))
		}
	}
	add := func(h string) {
		// Behind mode obtains no certificates: the front proxy owns TLS.
		if !model.Behind && !seen[h] && !certs.Covers(storedNames, h) {
			seen[h] = true
			status = append(status, hostCert(h, SourceACME, h == cfg.Domain, nil))
		}
	}
	if cfg.Domain != "" {
		add(cfg.Domain)
	}
	n := 0
	for _, r := range routes {
		if len(r.Servers) == 0 {
			continue
		}
		n++
		for _, d := range r.Domains {
			add(d)
		}
	}
	sort.Slice(status, func(i, j int) bool { return status[i].Hostname < status[j].Hostname })
	m.mu.Lock()
	m.certList, m.routes = status, n
	m.mu.Unlock()
	return nil
}

// edgeRunning reports whether the edge container exists and runs.
func (m *Manager) edgeRunning(ctx context.Context) (bool, error) {
	list, err := m.opt.Runtime.List(ctx, "")
	if err != nil {
		return false, fmt.Errorf("list containers: %w", err)
	}
	for _, c := range list {
		if c.Labels[LabelComponent] == ComponentEdge && c.State == "running" {
			return true, nil
		}
	}
	return false, nil
}

// projectRoutes resolves the exposures to routes with live backends. Routes
// without a running container are returned with no Servers (and skipped by
// [Dynamic]); invalid exposures are logged and dropped.
func (m *Manager) projectRoutes(ctx context.Context) ([]ProjectRoute, error) {
	exps, err := m.opt.Exposures.AllExposures(ctx)
	if err != nil {
		return nil, fmt.Errorf("load exposures: %w", err)
	}
	var out []ProjectRoute
	byProject := map[string][]runtime.Container{}
	for _, e := range exps {
		r := ProjectRoute{Project: e.Project, Service: e.Service, Domains: e.Domains, Path: e.Path}
		if _, err := r.Rule(); err != nil {
			m.log.Warn("edge: skipping invalid exposure", "project", e.Project, "service", e.Service, "err", err)
			continue
		}
		list, ok := byProject[e.Project]
		if !ok {
			list, err = m.opt.Runtime.List(ctx, e.Project)
			if err != nil {
				return nil, fmt.Errorf("list containers of %s: %w", e.Project, err)
			}
			byProject[e.Project] = list
		}
		for _, c := range list {
			if c.State != "running" || serviceOf(c) != e.Service {
				continue
			}
			d, err := m.opt.Runtime.Inspect(ctx, c.ID)
			if err != nil {
				if errors.Is(err, runtime.ErrNotFound) {
					continue
				}
				return nil, fmt.Errorf("inspect %s: %w", c.ID, err)
			}
			if ip := pickIP(d.IPs, e.Project); ip != "" {
				r.Servers = append(r.Servers, "http://"+net.JoinHostPort(ip, strconv.Itoa(e.Port)))
			}
		}
		sort.Strings(r.Servers)
		if len(r.Servers) == 0 {
			m.log.Debug("edge: no running backend, route omitted", "project", e.Project, "service", e.Service)
		}
		out = append(out, r)
	}
	return out, nil
}

func serviceOf(c runtime.Container) string {
	if s := c.Labels["com.docker.compose.service"]; s != "" {
		return s
	}
	return c.Service
}

// pickIP returns the IPv4 on a network named "<project>_*" if any, else the
// IPv4 of the alphabetically first network.
func pickIP(ips map[string]string, project string) string {
	names := make([]string, 0, len(ips))
	for n := range ips {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasPrefix(n, project+"_") {
			return ips[n]
		}
	}
	if len(names) > 0 {
		return ips[names[0]]
	}
	return ""
}

// writeIfChanged atomically replaces path with data unless it already holds
// exactly data. The temp file lives next to path and does not end in a
// configuration extension, so the proxy's directory watcher ignores it.
func writeIfChanged(path string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(name, path)
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func shortHash(s string) string { return hashOf([]byte(s))[:8] }

// shorten turns err into a single short line for the status.
func shorten(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > maxErrorLen {
		s = s[:maxErrorLen-3] + "..."
	}
	return s
}
