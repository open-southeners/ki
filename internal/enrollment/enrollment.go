// Package enrollment serves the enrollment.* JSON-RPC methods: it starts a
// device-flow enrollment against a control plane, waits for the operator to
// approve it, stores the resulting credentials and (re)starts the agent's
// outbound control-plane connection without a restart.
//
// The methods control the agent itself, so they are local-only (see
// rpc.LocalOnly). The access token and the device code never reach a log line,
// an error or a response.
package enrollment

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/controlplane"
	"github.com/open-southeners/ki-agent/internal/enroll"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/version"
)

// Method names.
const (
	MethodStart  = "enrollment.start"
	MethodWait   = "enrollment.wait"
	MethodRemove = "enrollment.remove"
)

// Sources reported in the control-plane status.
const (
	SourceConfig     = "config"
	SourceEnrollment = "enrollment"
)

// CredentialsFile is the credentials file name inside the data directory.
const CredentialsFile = "control-plane.json"

// Options configures Handlers.
type Options struct {
	// Client performs the device flow.
	Client *enroll.Client
	// Manager owns the control-plane connection.
	Manager *controlplane.Manager
	// Config supplies the control_plane.* settings other than url/token_file
	// (heartbeat, backoff, TLS) and the fallback when enrollment is removed.
	Config config.ControlPlane
	// DataDir holds the credentials file.
	DataDir string
	// DefaultURL is used when enrollment.start has no url (version.EnrollURL).
	DefaultURL string
	// Serve runs the JSON-RPC peer over a control-plane connection.
	Serve  func(ctx context.Context, c rpc.Conn)
	Logger *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Handlers serves the enrollment.* methods.
type Handlers struct {
	opts Options
	log  *slog.Logger
	path string
	now  func() time.Time

	// credMu serializes changes to the credentials file and the Manager
	// (finishing an enrollment, remove, startup).
	credMu sync.Mutex

	// mu guards cur.
	mu  sync.Mutex
	cur *run
}

// run is the enrollment started by one peer.
type run struct {
	peer    *rpc.Peer
	cancel  context.CancelFunc
	done    chan struct{} // closed once polling ended; valid after start returned
	started bool          // Start succeeded; guarded by Handlers.mu
	creds   enroll.Credentials
	err     error // set before done is closed
}

// New returns Handlers using opts.
func New(opts Options) *Handlers {
	h := &Handlers{opts: opts, log: opts.Logger, now: opts.Now}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	h.path = filepath.Join(opts.DataDir, CredentialsFile)
	return h
}

// Register installs the enrollment methods on r and makes them local-only.
// They sit behind whatever middleware r already has (the handshake gate).
func (h *Handlers) Register(r *rpc.Router) {
	r.Use(rpc.LocalOnly(MethodStart, MethodWait, MethodRemove))
	r.Handle(MethodStart, h.start)
	r.Handle(MethodWait, h.wait)
	r.Handle(MethodRemove, h.remove)
}

// connectorOptions builds the control-plane connector settings for url and
// token, taking everything else from the config.
func (h *Handlers) connectorOptions(url, token string) (*controlplane.Options, error) {
	cp := h.opts.Config
	tlsCfg, err := cp.TLSConfig()
	if err != nil {
		return nil, err
	}
	return &controlplane.Options{
		URL:               url,
		Token:             token,
		TLSConfig:         tlsCfg,
		HeartbeatInterval: cp.HeartbeatInterval,
		HeartbeatTimeout:  cp.HeartbeatTimeout,
		ReconnectMin:      cp.ReconnectMin,
		ReconnectMax:      cp.ReconnectMax,
		Serve:             h.opts.Serve,
		Logger:            h.log,
	}, nil
}

// configOptions builds the connector settings from the config's control plane,
// or nil when it is not configured.
func (h *Handlers) configOptions() (*controlplane.Options, error) {
	cp := h.opts.Config
	if !cp.Enabled() {
		return nil, nil
	}
	token, err := cp.Token()
	if err != nil {
		return nil, err
	}
	return h.connectorOptions(cp.URL, token)
}

// Apply configures the Manager at startup: the credentials file when present
// (it overrides the config's control plane), else the config's control plane
// when configured, else nothing. An unreadable credentials file is an error.
func (h *Handlers) Apply() error {
	h.credMu.Lock()
	defer h.credMu.Unlock()

	creds, err := enroll.LoadCredentials(h.path)
	switch {
	case err == nil:
		opts, err := h.connectorOptions(creds.URL, creds.Token)
		if err != nil {
			return err
		}
		if h.opts.Config.Enabled() {
			h.log.Info("enrollment credentials override the control_plane config",
				"credentials", h.path, "config_url", controlplane.SanitizeURL(h.opts.Config.URL))
		}
		h.log.Info("using enrolled control plane",
			"url", controlplane.SanitizeURL(creds.URL), "enrolled_at", creds.EnrolledAt)
		return h.opts.Manager.Apply(opts, SourceEnrollment)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	opts, err := h.configOptions()
	if err != nil {
		return err
	}
	return h.opts.Manager.Apply(opts, SourceConfig)
}

// mapError translates enrollment errors to JSON-RPC errors.
func mapError(err error) error {
	var ee *enroll.Error
	if errors.As(err, &ee) {
		msg := "enrollment error: " + ee.Reason
		if ee.Detail != "" {
			msg = "enrollment error: " + ee.Detail
		}
		return rpc.NewError(rpc.CodeEnrollmentError, msg).
			WithData(map[string]string{"reason": ee.Reason, "detail": ee.Detail})
	}
	return err
}

type startParams struct {
	URL   string `json:"url"`
	Force bool   `json:"force"`
}

func (h *Handlers) enrolled() bool {
	_, err := os.Stat(h.path)
	return err == nil
}

func (h *Handlers) start(ctx context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	var in startParams
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if in.URL == "" {
		in.URL = h.opts.DefaultURL
	}
	if in.URL == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: no enrollment URL: pass the control plane URL")
	}
	if h.enrolled() && !in.Force {
		return nil, rpc.NewError(rpc.CodeAlreadyExists, "already enrolled: pass force to enroll again, or remove the enrollment first")
	}

	// Reserve the single enrollment slot before the (slow) network calls. A
	// finished enrollment nobody collected does not block a new one.
	rctx, cancel := context.WithCancel(context.Background())
	r := &run{peer: p, cancel: cancel, done: make(chan struct{})}
	h.mu.Lock()
	if h.cur != nil && !h.cur.finished() {
		h.mu.Unlock()
		cancel()
		return nil, rpc.NewError(rpc.CodeAlreadyExists, "an enrollment is already in progress")
	}
	if h.cur != nil {
		h.cur.cancel()
	}
	h.cur = r
	h.mu.Unlock()

	// Bind the enrollment to the starting connection.
	go func() {
		select {
		case <-p.Done():
			cancel()
		case <-rctx.Done():
		}
	}()

	host, _ := os.Hostname()
	pending, err := h.opts.Client.Start(rctx, enroll.Request{
		BaseURL: in.URL, Hostname: host, OS: runtime.GOOS, Arch: runtime.GOARCH,
		AgentVersion: version.Version, Protocol: api.ProtocolVersion,
	})
	if err != nil {
		h.drop(r)
		return nil, mapError(err)
	}
	h.mu.Lock()
	r.started = true
	h.mu.Unlock()
	h.log.Info("enrollment started", "url", controlplane.SanitizeURL(in.URL),
		"user_code", pending.UserCode, "expires_at", pending.ExpiresAt)
	go h.poll(rctx, r, in.URL, pending)

	res := map[string]any{
		"user_code":        pending.UserCode,
		"verification_uri": pending.VerificationURI,
		"expires_at":       pending.ExpiresAt,
	}
	if pending.VerificationURIComplete != "" {
		res["verification_uri_complete"] = pending.VerificationURIComplete
	}
	return res, nil
}

// finished reports whether polling has ended. A run whose Start is still
// running is not finished. The caller holds Handlers.mu.
func (r *run) finished() bool {
	if !r.started {
		return false
	}
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

// drop cancels r and frees the slot if it still holds it.
func (h *Handlers) drop(r *run) {
	r.cancel()
	h.mu.Lock()
	if h.cur == r {
		h.cur = nil
	}
	h.mu.Unlock()
}

// poll waits for the approval and, on success, stores the credentials and
// starts the connection. It always closes r.done.
func (h *Handlers) poll(ctx context.Context, r *run, base string, pending *enroll.Pending) {
	defer close(r.done)
	defer r.cancel()

	creds, err := h.opts.Client.Wait(ctx, pending)
	if err != nil {
		r.err = err
		if ctx.Err() != nil {
			h.log.Info("enrollment cancelled")
			h.drop(r)
		} else {
			h.log.Warn("enrollment failed", "err", err)
		}
		return
	}
	creds.EnrollmentURL = base
	if creds.EnrolledAt.IsZero() {
		creds.EnrolledAt = h.now().UTC()
	}
	r.err = h.finish(ctx, creds)
	if r.err != nil {
		h.log.Warn("enrollment failed", "err", r.err)
		return
	}
	r.creds = creds
	h.log.Info("enrolled", "url", controlplane.SanitizeURL(creds.URL))
}

// finish stores creds and starts the connection. It does nothing when ctx was
// cancelled meanwhile (the peer went away or remove ran).
func (h *Handlers) finish(ctx context.Context, creds enroll.Credentials) error {
	h.credMu.Lock()
	defer h.credMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	opts, err := h.connectorOptions(creds.URL, creds.Token)
	if err != nil {
		return &enroll.Error{Reason: enroll.ReasonFailed, Detail: "control plane settings: " + err.Error()}
	}
	if err := enroll.SaveCredentials(h.path, creds); err != nil {
		return &enroll.Error{Reason: enroll.ReasonFailed, Detail: "store credentials: " + err.Error()}
	}
	if err := h.opts.Manager.Apply(opts, SourceEnrollment); err != nil && !errors.Is(err, controlplane.ErrManagerStopped) {
		return &enroll.Error{Reason: enroll.ReasonFailed, Detail: "start the control plane connection: " + err.Error()}
	}
	return nil
}

func (h *Handlers) wait(ctx context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
	h.mu.Lock()
	r := h.cur
	h.mu.Unlock()
	if r == nil || r.peer != p || !r.started {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: no enrollment in progress on this connection")
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	h.mu.Lock()
	if h.cur == r {
		h.cur = nil
	}
	h.mu.Unlock()
	if r.err != nil {
		if errors.Is(r.err, context.Canceled) {
			return nil, rpc.NewError(rpc.CodeEnrollmentError, "enrollment cancelled").
				WithData(map[string]string{"reason": enroll.ReasonFailed, "detail": "enrollment cancelled"})
		}
		return nil, mapError(r.err)
	}
	return map[string]string{"control_plane_url": r.creds.URL}, nil
}

func (h *Handlers) remove(_ context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
	// Stop any enrollment in progress; its polling must not recreate the file.
	h.mu.Lock()
	if h.cur != nil {
		h.cur.cancel()
		h.cur = nil
	}
	h.mu.Unlock()

	h.credMu.Lock()
	defer h.credMu.Unlock()
	if !h.enrolled() {
		return nil, rpc.NewError(rpc.CodeNotFound, "not enrolled")
	}
	// Build the fallback first so a broken config does not leave the agent
	// without credentials and without a connection.
	fallback, err := h.configOptions()
	if err != nil {
		return nil, err
	}
	if err := enroll.RemoveCredentials(h.path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, rpc.NewError(rpc.CodeNotFound, "not enrolled")
		}
		return nil, err
	}
	source := SourceConfig
	if err := h.opts.Manager.Apply(fallback, source); err != nil && !errors.Is(err, controlplane.ErrManagerStopped) {
		return nil, err
	}
	h.log.Info("enrollment removed", "fallback_to_config", fallback != nil)
	return struct{}{}, nil
}
