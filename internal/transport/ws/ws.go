// Package ws carries JSON-RPC over WebSocket text messages, with pluggable
// request authentication and a ping/pong heartbeat on either side.
package ws

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/open-southeners/ki/internal/rpc"
)

// Defaults applied for zero Options fields.
const (
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultHeartbeatTimeout  = 45 * time.Second
	DefaultReadLimit         = 4 << 20
)

// Options configures NewHandler.
type Options struct {
	// Authenticate vets the upgrade request. A non-nil error refuses it with
	// 401; the returned value is handed to the connection callback. Required.
	// The library's same-origin check on the Origin header stays active.
	Authenticate func(r *http.Request) (any, error)
	// HeartbeatInterval is the time between pings (default 15s).
	HeartbeatInterval time.Duration
	// HeartbeatTimeout is how long a pong may be late before the connection is
	// closed (default 45s).
	HeartbeatTimeout time.Duration
	// ReadLimit is the maximum inbound message size in bytes (default 4 MiB).
	ReadLimit int64
}

// ErrNoAuthenticate is returned by NewHandler when Options.Authenticate is nil.
var ErrNoAuthenticate = errors.New("ws: an Authenticate function is required")

// ErrUnauthorized is the error BearerAuth returns for a missing or wrong token.
var ErrUnauthorized = errors.New("ws: unauthorized")

// BearerAuth returns an Authenticate function accepting exactly the given
// non-empty bearer token (compared in constant time) and yielding nil.
func BearerAuth(token string) func(r *http.Request) (any, error) {
	want := []byte(token)
	return func(r *http.Request) (any, error) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(want) == 0 || !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			return nil, ErrUnauthorized
		}
		return nil, nil
	}
}

// NewHandler returns an http.Handler that authenticates the request, upgrades it
// to a WebSocket and invokes onConn with the connection and the value returned
// by Options.Authenticate. onConn runs on the
// handler goroutine; the connection is closed when it returns, and ctx is
// cancelled when the connection is closed or the heartbeat fails.
//
// The heartbeat pings every HeartbeatInterval and closes the connection if no
// pong arrives within HeartbeatTimeout. The WebSocket library only processes
// pongs while some goroutine is calling Read, so onConn (or the rpc.Peer it
// creates) must keep reading from the Conn for the lifetime of the connection.
func NewHandler(opts Options, onConn func(ctx context.Context, c rpc.Conn, authn any)) (http.Handler, error) {
	if opts.Authenticate == nil {
		return nil, ErrNoAuthenticate
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = DefaultHeartbeatTimeout
	}
	if opts.ReadLimit <= 0 {
		opts.ReadLimit = DefaultReadLimit
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authn, err := opts.Authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="ki-agent"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		wc, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // Accept has already written the error response.
		}
		wc.SetReadLimit(opts.ReadLimit)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		c := &conn{c: wc}
		go heartbeat(ctx, wc, opts.HeartbeatInterval, opts.HeartbeatTimeout)
		defer c.Close()
		onConn(ctx, c, authn)
	}), nil
}

func heartbeat(ctx context.Context, wc *websocket.Conn, interval, timeout time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, timeout)
		err := wc.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				// The peer is unresponsive, so a graceful close handshake
				// would only block; drop the connection immediately.
				_ = wc.CloseNow()
			}
			return
		}
	}
}

// DialOptions configures DialWith.
type DialOptions struct {
	// Token, when non-empty, is presented as "Authorization: Bearer <token>".
	Token string
	// Header carries extra request headers (for example User-Agent).
	Header http.Header
	// TLSConfig is used for wss:// (and https://) URLs; nil means the defaults.
	TLSConfig *tls.Config
	// HeartbeatInterval, when positive, makes the client ping the server at
	// that interval and drop the connection when no pong arrives within
	// HeartbeatTimeout (default 45s). Zero disables client-side pings.
	HeartbeatInterval time.Duration
	HeartbeatTimeout  time.Duration
	// ReadLimit is the maximum inbound message size in bytes (default 4 MiB).
	ReadLimit int64
}

// Dial opens a client WebSocket connection to url (ws://, wss://, http:// or
// https://) presenting token as a bearer credential.
func Dial(ctx context.Context, url, token string) (rpc.Conn, error) {
	c, _, err := DialWith(ctx, url, DialOptions{Token: token})
	return c, err
}

// DialWith opens a client WebSocket connection to url. ctx bounds only the
// handshake. When the handshake is rejected the HTTP response is returned
// alongside the error (with its body already consumed) so callers can tell a
// 401/403 from a network failure. With a heartbeat configured the connection
// is closed when the server stops answering pings; the rpc.Peer (or whoever
// owns the Conn) must keep reading so pongs are processed.
func DialWith(ctx context.Context, url string, opts DialOptions) (rpc.Conn, *http.Response, error) {
	h := opts.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	if opts.Token != "" {
		h.Set("Authorization", "Bearer "+opts.Token)
	}
	dopts := &websocket.DialOptions{HTTPHeader: h}
	if opts.TLSConfig != nil {
		dopts.HTTPClient = &http.Client{Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			TLSClientConfig: opts.TLSConfig.Clone(),
		}}
	}
	wc, resp, err := websocket.Dial(ctx, url, dopts)
	if err != nil {
		return nil, resp, err
	}
	limit := opts.ReadLimit
	if limit <= 0 {
		limit = DefaultReadLimit
	}
	wc.SetReadLimit(limit)
	c := &conn{c: wc}
	if opts.HeartbeatInterval > 0 {
		timeout := opts.HeartbeatTimeout
		if timeout <= 0 {
			timeout = DefaultHeartbeatTimeout
		}
		hctx, cancel := context.WithCancel(context.Background())
		c.stop = cancel
		go heartbeat(hctx, wc, opts.HeartbeatInterval, timeout)
	}
	return c, resp, nil
}

type conn struct {
	c    *websocket.Conn
	stop context.CancelFunc // ends the client heartbeat; nil on the server side
}

func (c *conn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := c.c.Read(ctx)
	return data, err
}

func (c *conn) Write(ctx context.Context, msg []byte) error {
	return c.c.Write(ctx, websocket.MessageText, msg)
}

func (c *conn) Close() error {
	if c.stop != nil {
		c.stop()
	}
	return c.c.Close(websocket.StatusNormalClosure, "")
}
