// Package ws carries JSON-RPC over WebSocket text messages, with bearer-token
// authentication and a server-side ping/pong heartbeat.
package ws

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

// Defaults applied for zero Options fields.
const (
	DefaultHeartbeatInterval = 15 * time.Second
	DefaultHeartbeatTimeout  = 45 * time.Second
	DefaultReadLimit         = 4 << 20
)

// Options configures NewHandler.
type Options struct {
	// Token is the required bearer token. It must be non-empty.
	Token string
	// HeartbeatInterval is the time between pings (default 15s).
	HeartbeatInterval time.Duration
	// HeartbeatTimeout is how long a pong may be late before the connection is
	// closed (default 45s).
	HeartbeatTimeout time.Duration
	// ReadLimit is the maximum inbound message size in bytes (default 4 MiB).
	ReadLimit int64
}

// ErrEmptyToken is returned by NewHandler when no token is configured.
var ErrEmptyToken = errors.New("ws: a non-empty bearer token is required")

// NewHandler returns an http.Handler that authenticates the request, upgrades it
// to a WebSocket and invokes onConn with the connection. onConn runs on the
// handler goroutine; the connection is closed when it returns, and ctx is
// cancelled when the connection is closed or the heartbeat fails.
//
// The heartbeat pings every HeartbeatInterval and closes the connection if no
// pong arrives within HeartbeatTimeout. The WebSocket library only processes
// pongs while some goroutine is calling Read, so onConn (or the rpc.Peer it
// creates) must keep reading from the Conn for the lifetime of the connection.
func NewHandler(opts Options, onConn func(ctx context.Context, c rpc.Conn)) (http.Handler, error) {
	if opts.Token == "" {
		return nil, ErrEmptyToken
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
	want := []byte(opts.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
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
		onConn(ctx, c)
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

// Dial opens a client WebSocket connection to url (ws://, wss://, http:// or
// https://) presenting token as a bearer credential.
func Dial(ctx context.Context, url, token string) (rpc.Conn, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	wc, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		return nil, err
	}
	wc.SetReadLimit(DefaultReadLimit)
	return &conn{c: wc}, nil
}

type conn struct{ c *websocket.Conn }

func (c *conn) Read(ctx context.Context) ([]byte, error) {
	_, data, err := c.c.Read(ctx)
	return data, err
}

func (c *conn) Write(ctx context.Context, msg []byte) error {
	return c.c.Write(ctx, websocket.MessageText, msg)
}

func (c *conn) Close() error {
	return c.c.Close(websocket.StatusNormalClosure, "")
}
