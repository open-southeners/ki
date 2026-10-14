// Package server accepts connections on the Unix socket and the optional
// WebSocket listener and serves each with an rpc.Peer.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/unixsock"
	"github.com/open-southeners/ki-agent/internal/transport/ws"
)

// DefaultShutdownTimeout bounds the HTTP server shutdown.
const DefaultShutdownTimeout = 5 * time.Second

// Options configures a Server.
type Options struct {
	Config *config.Config
	Router *rpc.Router
	Logger *slog.Logger
	// ShutdownTimeout bounds graceful HTTP shutdown (default 5s).
	ShutdownTimeout time.Duration
}

// Server owns the listeners and the set of live peers.
type Server struct {
	cfg     *config.Config
	router  *rpc.Router
	log     *slog.Logger
	timeout time.Duration

	wsHandler http.Handler

	unix   *unixsock.Listener
	wsLn   net.Listener
	httpSv *http.Server

	mu     sync.Mutex
	peers  map[*rpc.Peer]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New validates the options and prepares (but does not bind) the server. It
// reads the WebSocket token when the WebSocket listener is configured.
func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.Router == nil {
		return nil, errors.New("server: config and router are required")
	}
	s := &Server{
		cfg:     opts.Config,
		router:  opts.Router,
		log:     opts.Logger,
		timeout: opts.ShutdownTimeout,
		peers:   make(map[*rpc.Peer]struct{}),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.timeout <= 0 {
		s.timeout = DefaultShutdownTimeout
	}
	if w := s.cfg.WebSocket; w.Enabled() {
		token, err := w.Token()
		if err != nil {
			return nil, err
		}
		h, err := ws.NewHandler(ws.Options{
			Token:             token,
			HeartbeatInterval: w.HeartbeatInterval,
			HeartbeatTimeout:  w.HeartbeatTimeout,
		}, func(ctx context.Context, c rpc.Conn) { s.servePeer(ctx, "websocket", c) })
		if err != nil {
			return nil, err
		}
		s.wsHandler = h
	}
	return s, nil
}

// Listen binds the Unix socket (creating its directory if needed) and, if
// configured, the WebSocket TCP listener.
func (s *Server) Listen() error {
	sock := s.cfg.Socket
	if err := os.MkdirAll(filepath.Dir(sock.Path), 0o750); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	ul, err := unixsock.Listen(sock.Path, os.FileMode(sock.Mode))
	if err != nil {
		return fmt.Errorf("listen on socket: %w", err)
	}
	s.unix = ul
	s.log.Info("listening", "transport", "unix", "addr", ul.Addr())

	if s.wsHandler == nil {
		return nil
	}
	ln, err := net.Listen("tcp", s.cfg.WebSocket.Listen)
	if err != nil {
		_ = ul.Close()
		return fmt.Errorf("listen on websocket address: %w", err)
	}
	s.wsLn = ln
	mux := http.NewServeMux()
	mux.Handle(s.cfg.WebSocket.Path, s.wsHandler)
	s.httpSv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.log.Info("listening", "transport", "websocket", "addr", ln.Addr().String(), "path", s.cfg.WebSocket.Path)
	return nil
}

// WebSocketAddr returns the bound WebSocket address, or "" if disabled or not
// yet listening.
func (s *Server) WebSocketAddr() string {
	if s.wsLn == nil {
		return ""
	}
	return s.wsLn.Addr().String()
}

// Serve accepts connections until ctx is cancelled, then stops listening,
// closes every peer and waits for them to finish. Listen must have succeeded.
// It returns nil on a clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	httpErr := make(chan error, 1)
	if s.httpSv != nil {
		go func() { httpErr <- s.httpSv.Serve(s.wsLn) }()
	}
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		s.acceptUnix(ctx)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-httpErr:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = fmt.Errorf("websocket server: %w", err)
		}
	}
	return errors.Join(serveErr, s.shutdown(ctx, acceptDone))
}

func (s *Server) shutdown(ctx context.Context, acceptDone <-chan struct{}) error {
	var errs []error
	if err := s.unix.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, err)
	}
	<-acceptDone

	s.mu.Lock()
	s.closed = true
	peers := make([]*rpc.Peer, 0, len(s.peers))
	for p := range s.peers {
		peers = append(peers, p)
	}
	s.mu.Unlock()
	for _, p := range peers {
		_ = p.Close()
	}

	if s.httpSv != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
		if err := s.httpSv.Shutdown(sctx); err != nil {
			_ = s.httpSv.Close()
			errs = append(errs, fmt.Errorf("websocket shutdown: %w", err))
		}
		cancel()
	}
	s.wg.Wait()
	return errors.Join(errs...)
}

func (s *Server) acceptUnix(ctx context.Context) {
	for {
		c, err := s.unix.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.log.Error("accept failed", "transport", "unix", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.servePeer(ctx, "unix", c)
		}()
	}
}

// ServeConn serves the JSON-RPC API over c until the connection ends and
// returns then. It applies the same peer options, handshake gate and logging
// as connections accepted by the listeners, and is tracked for graceful
// shutdown: Serve closes the peer and waits for ServeConn to return. After
// shutdown has begun, c is closed immediately. transport labels log lines.
// A hook installed with agent.WithHelloHook on ctx is called after each
// successful agent.hello on this connection.
func (s *Server) ServeConn(ctx context.Context, transport string, c rpc.Conn) {
	s.servePeer(ctx, transport, c)
}

// servePeer runs a Peer over c until it ends. It is also the WebSocket onConn
// callback, so it must not return before the connection is done.
func (s *Server) servePeer(ctx context.Context, transport string, c rpc.Conn) {
	p := rpc.NewPeer(c, s.router, rpc.WithLogger(s.log),
		// Serial until handshaken so requests pipelined behind agent.hello see its effect.
		rpc.WithSerialDispatch(func(p *rpc.Peer) bool { return !agent.Handshaken(p) }))

	if fn := agent.HelloHook(ctx); fn != nil {
		agent.SetHelloHook(p, fn)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return
	}
	s.peers[p] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.peers, p)
		s.mu.Unlock()
		s.wg.Done()
	}()

	start := time.Now()
	s.log.Info("connection opened", "transport", transport)
	err := p.Run(ctx)
	_ = p.Close()
	attrs := []any{"transport", transport, "duration", time.Since(start).Round(time.Millisecond)}
	if err != nil && !errors.Is(err, context.Canceled) {
		attrs = append(attrs, "err", err)
	}
	s.log.Info("connection closed", attrs...)
}
