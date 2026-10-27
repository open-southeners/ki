package edge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/open-southeners/ki/internal/edge/certs"
	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/store"
)

// RPC method names.
const (
	MethodStatus  = "edge.status"
	MethodCSR     = "edge.csr"
	MethodInstall = "edge.certificate.install"
	MethodRemove  = "edge.certificate.remove"
	// MethodImport imports a bring-your-own certificate. It carries a
	// private key, so it is limited to the local socket.
	MethodImport = "edge.certificate.import"
)

const paramsPrefix = "invalid params: "

// StatusSource is the part of the [Manager] the handlers use.
type StatusSource interface {
	Status() Status
	Trigger()
}

// DomainOwner reports which project and service expose domain. It returns
// store.ErrNotFound when the domain is free.
type DomainOwner func(ctx context.Context, domain string) (project, service string, err error)

// Handlers serves the edge.* methods.
type Handlers struct {
	// DomainOwner, when set, is consulted so that a hostname exposed by a
	// project cannot also become a server hostname.
	DomainOwner DomainOwner

	mgr StatusSource
	dir string
	log *slog.Logger
	now func() time.Time
}

// NewHandlers returns handlers for the edge in dir (<data_dir>/edge).
func NewHandlers(mgr StatusSource, dir string, logger *slog.Logger) *Handlers {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handlers{mgr: mgr, dir: dir, log: logger, now: time.Now}
}

// Register installs the edge methods on r. Certificate management is limited
// to the local socket and the control plane; the status is open to all.
func (h *Handlers) Register(r *rpc.Router) {
	r.Use(rpc.AllowTransports([]string{MethodCSR, MethodInstall, MethodRemove},
		rpc.TransportUnix, rpc.TransportControlPlane))
	r.Use(rpc.AllowTransports([]string{MethodImport}, rpc.TransportUnix))
	r.Handle(MethodImport, h.importCert)
	r.Handle(MethodStatus, h.status)
	r.Handle(MethodCSR, h.csr)
	r.Handle(MethodInstall, h.install)
	r.Handle(MethodRemove, h.remove)
}

func (h *Handlers) status(context.Context, *rpc.Peer, json.RawMessage) (any, error) {
	return h.mgr.Status(), nil
}

type hostnameParams struct {
	Hostname string `json:"hostname"`
}

// checkHostname validates hostname and refuses one that a project exposes.
func (h *Handlers) checkHostname(ctx context.Context, hostname string) error {
	if err := certs.ValidHostname(hostname); err != nil {
		return rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
	}
	if h.DomainOwner == nil {
		return nil
	}
	project, service, err := h.DomainOwner(ctx, hostname)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	return rpc.NewError(rpc.CodeAlreadyExists,
		fmt.Sprintf("%s is exposed by project %q (service %q); it cannot be a server hostname", hostname, project, service)).
		WithData(map[string]string{"domain": hostname, "project": project, "service": service})
}

func (h *Handlers) csr(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var p hostnameParams
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := h.checkHostname(ctx, p.Hostname); err != nil {
		return nil, err
	}
	pemCSR, err := certs.CSR(h.dir, p.Hostname)
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
	}
	return struct {
		Hostname string `json:"hostname"`
		CSR      string `json:"csr"`
	}{p.Hostname, string(pemCSR)}, nil
}

func (h *Handlers) install(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var p struct {
		Hostname    string `json:"hostname"`
		Certificate string `json:"certificate"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if err := h.checkHostname(ctx, p.Hostname); err != nil {
		return nil, err
	}
	if p.Certificate == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+"certificate is required")
	}
	if _, err := certs.Install(h.dir, p.Hostname, []byte(p.Certificate), h.now()); err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
	}
	h.log.Info("edge certificate installed", "hostname", p.Hostname)
	h.mgr.Trigger()
	return h.mgr.Status(), nil
}

func (h *Handlers) importCert(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var p struct {
		Certificate string `json:"certificate"`
		Key         string `json:"key"`
		Server      bool   `json:"server"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.Certificate == "" || p.Key == "" {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+"certificate and key are required")
	}
	names, err := certs.ImportNames([]byte(p.Certificate), []byte(p.Key), h.now())
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
	}
	if p.Server {
		// Server hostnames keep the reserved-hostname rule; wildcards never
		// conflict with project domains.
		for _, n := range names {
			if strings.HasPrefix(n, "*.") {
				continue
			}
			if err := h.checkHostname(ctx, n); err != nil {
				return nil, err
			}
		}
	}
	imp, err := certs.Import(h.dir, []byte(p.Certificate), []byte(p.Key), p.Server, h.now())
	if err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
	}
	h.log.Info("edge certificate imported", "id", imp.ID, "names", imp.Names, "server", imp.Server)
	h.mgr.Trigger()
	return h.mgr.Status(), nil
}

func (h *Handlers) remove(ctx context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
	var p struct {
		Hostname string `json:"hostname"`
		ID       string `json:"id"`
	}
	if err := rpc.DecodeParams(params, &p); err != nil {
		return nil, err
	}
	if p.ID != "" {
		if err := certs.RemoveImported(h.dir, p.ID); err != nil {
			return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
		}
		h.log.Info("edge certificate removed", "id", p.ID)
		h.mgr.Trigger()
		return h.mgr.Status(), nil
	}
	if err := h.checkHostname(ctx, p.Hostname); err != nil {
		return nil, err
	}
	if err := certs.Remove(h.dir, p.Hostname); err != nil {
		return nil, rpc.NewError(rpc.CodeInvalidParams, paramsPrefix+err.Error())
	}
	h.log.Info("edge certificate removed", "hostname", p.Hostname)
	h.mgr.Trigger()
	return h.mgr.Status(), nil
}
