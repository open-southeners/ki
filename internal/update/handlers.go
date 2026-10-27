package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki/internal/auth"
	"github.com/open-southeners/ki/internal/rpc"
)

// RPC method names.
const (
	MethodStatus = "updates.status"
	MethodCheck  = "updates.check"
	MethodApply  = "updates.apply"
)

const (
	// pollInterval is how often the result file is looked at while a
	// request is outstanding.
	pollInterval = 2 * time.Second
	// requestTimeout is how long the helper gets to produce a result.
	requestTimeout = 10 * time.Minute

	// seenFile remembers the id of the last result that was logged and
	// audited, so a restart does not report it again.
	seenFile = "result.seen"

	helperMissingMsg = "the update helper did not run; is ki-agent-update.path enabled?"
	applyRefusedMsg  = "updates are installed from the panel or with sudo ki agent update"

	// helperUnit is the systemd path unit that makes installing from the
	// panel possible.
	helperUnit = "ki-agent-update.path"
)

var helperUnitDirs = []string{"/etc/systemd/system", "/lib/systemd/system", "/usr/lib/systemd/system"}

// HelperInstalled reports whether the root helper's path unit is installed.
func HelperInstalled() bool {
	for _, d := range helperUnitDirs {
		if _, err := os.Stat(filepath.Join(d, helperUnit)); err == nil {
			return true
		}
	}
	return false
}

// StatusSource is the part of the [Checker] the handlers use.
type StatusSource interface {
	Status() UpdateStatus
	CheckNow(ctx context.Context) UpdateStatus
}

// HandlerOptions configures [Handlers].
type HandlerOptions struct {
	// Source supplies the release information.
	Source StatusSource
	// Dir is the exchange directory shared with the root helper.
	Dir    string
	Logger *slog.Logger
	// Audit records an event in the audit log; nil disables it.
	Audit func(ctx context.Context, event, target, result string)
	// ApplySupported reports whether the root helper is installed; nil
	// selects [HelperInstalled].
	ApplySupported func() bool
	// Now, PollInterval and RequestTimeout override the clock and timings
	// (tests).
	Now            func() time.Time
	PollInterval   time.Duration
	RequestTimeout time.Duration
}

// Handlers serves the updates.* methods and follows an outstanding update
// request until the helper reports its result.
type Handlers struct {
	opts HandlerOptions
	log  *slog.Logger

	mu       sync.Mutex // guards the fields below
	state    string
	reqID    string
	reqAt    time.Time
	last     *Result
	seenID   string
	seenInit bool
}

// NewHandlers returns handlers for the updates.* methods.
func NewHandlers(o HandlerOptions) *Handlers {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.ApplySupported == nil {
		o.ApplySupported = HelperInstalled
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PollInterval <= 0 {
		o.PollInterval = pollInterval
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = requestTimeout
	}
	return &Handlers{opts: o, log: o.Logger, state: StateIdle}
}

// Register installs the updates methods on r. Applying is limited to panel
// sessions inside the handler, because that needs the principal.
func (h *Handlers) Register(r *rpc.Router) {
	r.Handle(MethodStatus, h.status)
	r.Handle(MethodCheck, h.check)
	r.Handle(MethodApply, h.apply)
}

// Run reports the result of an earlier update once (logged and audited), then
// polls for the result of requests until ctx ends.
func (h *Handlers) Run(ctx context.Context) {
	h.reportExisting(ctx)
	t := time.NewTicker(h.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.poll(ctx)
		}
	}
}

// reportExisting loads the result left by a previous update, if any, and logs
// and audits it unless it was already reported.
func (h *Handlers) reportExisting(ctx context.Context) {
	res, err := ReadResult(h.opts.Dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			h.log.Warn("read update result", "err", err)
		}
		return
	}
	h.mu.Lock()
	h.last = &res
	h.mu.Unlock()
	h.report(ctx, res)
}

// report logs and audits res unless its id was reported before.
func (h *Handlers) report(ctx context.Context, res Result) {
	h.mu.Lock()
	if !h.seenInit {
		b, _ := os.ReadFile(filepath.Join(h.opts.Dir, seenFile))
		h.seenID = strings.TrimSpace(string(b))
		h.seenInit = true
	}
	if res.ID != "" && res.ID == h.seenID {
		h.mu.Unlock()
		return
	}
	h.seenID = res.ID
	h.mu.Unlock()
	if err := os.WriteFile(filepath.Join(h.opts.Dir, seenFile), []byte(res.ID+"\n"), 0o640); err != nil {
		h.log.Warn("remember update result", "err", err)
	}

	attrs := []any{"id", res.ID, "status", res.Status, "from", res.From, "to", res.To}
	if res.Error != "" {
		attrs = append(attrs, "error", res.Error)
	}
	if res.Status == ResultSucceeded {
		h.log.Info("update finished", attrs...)
	} else {
		h.log.Warn("update did not succeed", attrs...)
	}
	if h.opts.Audit != nil {
		h.opts.Audit(ctx, "updates.result", res.To, res.Status)
	}
}

// poll looks for the result of the outstanding request and gives up on it
// after the timeout.
func (h *Handlers) poll(ctx context.Context) {
	h.mu.Lock()
	if h.state != StateRequested {
		h.mu.Unlock()
		return
	}
	id, at := h.reqID, h.reqAt
	h.mu.Unlock()

	res, err := ReadResult(h.opts.Dir)
	if err == nil && res.ID == id {
		h.finish(ctx, id, res)
		return
	}
	if now := h.opts.Now(); now.Sub(at) >= h.opts.RequestTimeout {
		h.finish(ctx, id, Result{ID: id, Status: ResultFailed, StartedAt: at, FinishedAt: now.UTC(), Error: helperMissingMsg})
	}
}

// finish records res as the outcome of request id and returns to idle.
func (h *Handlers) finish(ctx context.Context, id string, res Result) {
	h.mu.Lock()
	if h.state != StateRequested || h.reqID != id {
		h.mu.Unlock()
		return
	}
	h.state = StateIdle
	h.last = &res
	h.mu.Unlock()
	h.report(ctx, res)
}

// snapshot merges the checker's status with the apply flow's fields.
func (h *Handlers) snapshot(st UpdateStatus) UpdateStatus {
	st.ApplySupported = h.opts.ApplySupported()
	h.mu.Lock()
	defer h.mu.Unlock()
	st.State = h.state
	if h.last != nil {
		r := *h.last
		st.LastResult = &r
	}
	return st
}

func (h *Handlers) status(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
	return h.snapshot(h.opts.Source.Status()), nil
}

func (h *Handlers) check(ctx context.Context, _ *rpc.Peer, _ json.RawMessage) (any, error) {
	return h.snapshot(h.opts.Source.CheckNow(ctx)), nil
}

func (h *Handlers) apply(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
	pr, _ := auth.PrincipalOf(p)
	if rpc.TransportOf(p) != rpc.TransportWebSocket || pr.Kind != auth.KindUser {
		return nil, rpc.NewError(rpc.CodeUnauthorized, applyRefusedMsg)
	}
	var in struct {
		Version string `json:"version"`
	}
	if err := rpc.DecodeParams(params, &in); err != nil {
		return nil, err
	}
	if !ValidVersion(in.Version) {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: version must look like v1.2.3")
	}
	st := h.opts.Source.Status()
	if !st.UpdateAvailable || st.Latest == nil || st.Latest.Tag != in.Version {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "invalid params: "+in.Version+" is not the latest available release")
	}
	if !h.opts.ApplySupported() {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "installing from the panel is not set up on this server; run sudo ki agent update")
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == StateRequested {
		return nil, rpc.NewError(rpc.CodeInvalidParams, "an update is already in progress")
	}
	now := h.opts.Now().UTC()
	req := Request{ID: newID(), Version: in.Version, RequestedBy: pr.Name, RequestedAt: now}
	if err := WriteRequest(h.opts.Dir, req); err != nil {
		return nil, err
	}
	h.state = StateRequested
	h.reqID = req.ID
	h.reqAt = now
	h.log.Info("update requested", "version", in.Version, "by", pr.Name, "id", req.ID)
	return map[string]string{"id": req.ID, "state": StateRequested}, nil
}
