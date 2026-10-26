// Package server accepts connections on the Unix socket and the loopback HTTP
// listeners (direct and edge) and serves each with an authenticated rpc.Peer.
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
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/internal/agent"
	"github.com/open-southeners/ki-agent/internal/auth"
	"github.com/open-southeners/ki-agent/internal/config"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/unixsock"
	"github.com/open-southeners/ki-agent/internal/transport/ws"
)

// DefaultShutdownTimeout bounds the HTTP server shutdown.
const DefaultShutdownTimeout = 5 * time.Second

// SessionCookie is the name of the cookie carrying a panel session id.
const SessionCookie = "__Host-ki_session"

// RPCPath is the HTTP path of the WebSocket JSON-RPC endpoint.
const RPCPath = "/rpc"

// Authenticator resolves remote credentials to principals. Implementations
// return an error for unknown, expired or revoked credentials.
type Authenticator interface {
	// Session resolves the value of the session cookie.
	Session(ctx context.Context, sessionID string) (auth.Principal, error)
	// Token resolves an API token presented as a bearer credential.
	Token(ctx context.Context, bearer string) (auth.Principal, error)
}

// Options configures a Server.
type Options struct {
	Config *config.Config
	Router *rpc.Router
	Logger *slog.Logger
	// ShutdownTimeout bounds graceful HTTP shutdown (default 5s).
	ShutdownTimeout time.Duration
	// Auth authenticates remote (WebSocket) clients. When nil every remote
	// request is refused; the Unix socket and control plane are unaffected.
	Auth Authenticator
	// HTTP, when set, serves everything except /rpc on both listeners (the
	// panel and its /auth endpoints).
	HTTP http.Handler
}

// listener is one bound HTTP listener.
type listener struct {
	name string // "direct" or "edge"
	edge bool
	addr string
	ln   net.Listener
	srv  *http.Server
}

// peerInfo is the per-connection authentication result handed from the
// WebSocket handshake to servePeer.
type peerInfo struct {
	principal auth.Principal
	ip        string
}

// Server owns the listeners and the set of live peers.
type Server struct {
	cfg     *config.Config
	router  *rpc.Router
	log     *slog.Logger
	timeout time.Duration
	auth    Authenticator
	http    http.Handler

	unix      *unixsock.Listener
	listeners []*listener

	mu     sync.Mutex
	peers  map[*rpc.Peer]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New validates the options and prepares (but does not bind) the server. It
// installs role enforcement (auth.RequireRoles over auth.MethodRoles) on the
// router, so every method registered on it is subject to the role table.
func New(opts Options) (*Server, error) {
	if opts.Config == nil || opts.Router == nil {
		return nil, errors.New("server: config and router are required")
	}
	s := &Server{
		cfg:     opts.Config,
		router:  opts.Router,
		log:     opts.Logger,
		timeout: opts.ShutdownTimeout,
		auth:    opts.Auth,
		http:    opts.HTTP,
		peers:   make(map[*rpc.Peer]struct{}),
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.timeout <= 0 {
		s.timeout = DefaultShutdownTimeout
	}
	s.router.Use(auth.RequireRoles(auth.MethodRoles))

	s.listeners = append(s.listeners, &listener{name: "direct", addr: s.cfg.API.Listen})
	if s.cfg.Edge.Enabled {
		s.listeners = append(s.listeners, &listener{name: "edge", edge: true, addr: s.cfg.API.EdgeListen})
	}
	for _, l := range s.listeners {
		h, err := ws.NewHandler(ws.Options{
			Authenticate:      s.authenticator(l.edge),
			HeartbeatInterval: s.cfg.API.HeartbeatInterval,
			HeartbeatTimeout:  s.cfg.API.HeartbeatTimeout,
		}, func(ctx context.Context, c rpc.Conn, authn any) {
			info := authn.(peerInfo)
			s.servePeer(ctx, rpc.TransportWebSocket, c, info.principal, info.ip)
		})
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		if s.http != nil {
			mux.Handle("/", markEdge(s.http, l.edge))
		}
		mux.Handle(RPCPath, h)
		l.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	}
	return s, nil
}

type fromEdgeKey struct{}

// markEdge records in the request context which listener kind served it.
func markEdge(h http.Handler, edge bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(WithEdge(r.Context(), edge)))
	})
}

// WithEdge returns ctx marked as coming from the edge (or direct) listener.
func WithEdge(ctx context.Context, edge bool) context.Context {
	return context.WithValue(ctx, fromEdgeKey{}, edge)
}

// FromEdge reports whether a request handled by Options.HTTP arrived on the
// edge listener (behind the trusted reverse proxy) rather than the direct one.
func FromEdge(ctx context.Context) bool {
	v, _ := ctx.Value(fromEdgeKey{}).(bool)
	return v
}

// authenticator builds the /rpc handshake policy for a listener kind.
//
// Direct: a bearer token, or the session cookie when the panel is enabled.
// Edge: the session cookie (panel enabled), and a bearer token only when
// api.public_tokens is set. A bearer credential that the listener does not
// accept is refused outright rather than falling back to a cookie. The client
// IP comes from the last X-Forwarded-For hop on the edge listener only.
func (s *Server) authenticator(edge bool) func(r *http.Request) (any, error) {
	tokens := !edge || s.cfg.API.PublicTokens
	cookies := s.cfg.Panel.Enabled
	return func(r *http.Request) (any, error) {
		if s.auth == nil {
			return nil, errors.New("remote authentication is not configured")
		}
		var (
			pr  auth.Principal
			err error
		)
		if bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			if !tokens || bearer == "" {
				return nil, errors.New("bearer tokens are not accepted here")
			}
			pr, err = s.auth.Token(r.Context(), bearer)
		} else if c, cerr := r.Cookie(SessionCookie); cerr == nil && cookies && c.Value != "" {
			pr, err = s.auth.Session(r.Context(), c.Value)
		} else {
			return nil, errors.New("no credentials")
		}
		if err != nil {
			return nil, err
		}
		return peerInfo{principal: pr, ip: auth.ClientIP(r, edge)}, nil
	}
}

// Listen binds the Unix socket (creating its directory if needed) and the TCP
// listeners: api.listen, plus api.edge_listen when the edge is enabled.
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

	for _, l := range s.listeners {
		ln, err := net.Listen("tcp", l.addr)
		if err != nil {
			_ = ul.Close()
			for _, b := range s.listeners {
				if b.ln != nil {
					_ = b.ln.Close()
				}
			}
			return fmt.Errorf("listen on %s api address: %w", l.name, err)
		}
		l.ln = ln
		s.log.Info("listening", "transport", "websocket", "listener", l.name, "addr", ln.Addr().String(), "path", RPCPath)
	}
	return nil
}

// APIAddr returns the bound direct API address, or "" before Listen.
func (s *Server) APIAddr() string { return s.addrOf(false) }

// EdgeAPIAddr returns the bound edge API address, or "" if the edge is
// disabled or the server is not yet listening.
func (s *Server) EdgeAPIAddr() string { return s.addrOf(true) }

func (s *Server) addrOf(edge bool) string {
	for _, l := range s.listeners {
		if l.edge == edge && l.ln != nil {
			return l.ln.Addr().String()
		}
	}
	return ""
}

// CloseUser closes every live peer authenticated as the user with the given id.
func (s *Server) CloseUser(userID int64) { s.closePeers(auth.KindUser, userID) }

// CloseToken closes every live peer authenticated with the API token of the
// given id.
func (s *Server) CloseToken(tokenID int64) { s.closePeers(auth.KindToken, tokenID) }

func (s *Server) closePeers(kind string, id int64) {
	s.mu.Lock()
	var victims []*rpc.Peer
	for p := range s.peers {
		if pr, ok := auth.PrincipalOf(p); ok && pr.Kind == kind && pr.ID == id {
			victims = append(victims, p)
		}
	}
	s.mu.Unlock()
	for _, p := range victims {
		_ = p.Close()
	}
}

// Serve accepts connections until ctx is cancelled, then stops listening,
// closes every peer and waits for them to finish. Listen must have succeeded.
// It returns nil on a clean shutdown.
func (s *Server) Serve(ctx context.Context) error {
	httpErr := make(chan error, len(s.listeners))
	for _, l := range s.listeners {
		go func() { httpErr <- l.srv.Serve(l.ln) }()
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
			serveErr = fmt.Errorf("api server: %w", err)
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

	for _, l := range s.listeners {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.timeout)
		if err := l.srv.Shutdown(sctx); err != nil {
			_ = l.srv.Close()
			errs = append(errs, fmt.Errorf("%s api shutdown: %w", l.name, err))
		}
		cancel()
	}
	s.wg.Wait()
	return errors.Join(errs...)
}

func (s *Server) acceptUnix(ctx context.Context) {
	local := auth.Principal{Kind: auth.KindLocal, Name: "local", Role: auth.RoleAdmin}
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
			s.servePeer(ctx, rpc.TransportUnix, c, local, "")
		}()
	}
}

// ServeConn serves the JSON-RPC API over c until the connection ends and
// returns then. It applies the same peer options, handshake gate and logging
// as connections accepted by the listeners, and is tracked for graceful
// shutdown: Serve closes the peer and waits for ServeConn to return. After
// shutdown has begun, c is closed immediately. transport labels log lines and
// is recorded on the peer (see rpc.TransportOf) so local-only methods can be
// refused on other transports. The peer acts as the control-plane principal
// (admin).
// A hook installed with agent.WithHelloHook on ctx is called after each
// successful agent.hello on this connection.
func (s *Server) ServeConn(ctx context.Context, transport string, c rpc.Conn) {
	cp := auth.Principal{Kind: auth.KindControlPlane, Name: "control-plane", Role: auth.RoleAdmin}
	s.servePeer(ctx, transport, c, cp, "")
}

// servePeer runs a Peer acting as pr over c until it ends. It is also the
// WebSocket onConn callback, so it must not return before the connection is
// done.
func (s *Server) servePeer(ctx context.Context, transport string, c rpc.Conn, pr auth.Principal, ip string) {
	p := rpc.NewPeer(c, s.router, rpc.WithLogger(s.log),
		// Serial until handshaken so requests pipelined behind agent.hello see its effect.
		rpc.WithSerialDispatch(func(p *rpc.Peer) bool { return !agent.Handshaken(p) }))

	rpc.SetTransport(p, transport)
	auth.SetPrincipal(p, pr)
	auth.SetClientIP(p, ip)
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
	s.log.Info("connection opened", "transport", transport, "principal", pr.Kind+":"+pr.Name)
	err := p.Run(ctx)
	_ = p.Close()
	attrs := []any{"transport", transport, "duration", time.Since(start).Round(time.Millisecond)}
	if err != nil && !errors.Is(err, context.Canceled) {
		attrs = append(attrs, "err", err)
	}
	s.log.Info("connection closed", attrs...)
}
