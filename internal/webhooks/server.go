package webhooks

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

const shutdownTimeout = 10 * time.Second

// Server runs the webhook Handler on its own listener.
type Server struct {
	Addr        string
	TLSCertFile string // with TLSKeyFile, serves HTTPS
	TLSKeyFile  string
	Handler     http.Handler
	Logger      *slog.Logger
}

// ListenAndServe listens on Addr and serves until ctx is cancelled, then
// shuts down gracefully. It returns nil after a clean shutdown.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Serve is ListenAndServe on an existing listener.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	srv := &http.Server{
		Handler:           s.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	errc := make(chan error, 1)
	go func() {
		if s.TLSCertFile != "" && s.TLSKeyFile != "" {
			errc <- srv.ServeTLS(ln, s.TLSCertFile, s.TLSKeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	log.Info("webhook listener started", "addr", ln.Addr().String(), "tls", s.TLSCertFile != "")

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
		return err
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
