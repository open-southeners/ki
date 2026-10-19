// Package client is a small JSON-RPC client for the agent's local Unix socket,
// used by the ki-agent command-line interface. It performs the agent.hello
// handshake and delivers subscription updates in sequence order.
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-southeners/ki-agent/api"
	"github.com/open-southeners/ki-agent/internal/rpc"
	"github.com/open-southeners/ki-agent/internal/transport/unixsock"
	"github.com/open-southeners/ki-agent/internal/version"
)

const (
	// clientName identifies the CLI in agent.hello.
	clientName = "ki-agent-cli"
	// maxEarlyUpdates bounds how many updates are held for subscription ids
	// whose subscribe response has not arrived yet.
	maxEarlyUpdates = 4096
	// gapWait is how long a missing sequence number is waited for before the
	// updates after it are delivered anyway (snapshot subscriptions skip seqs).
	gapWait = 500 * time.Millisecond
)

// Client is a connected, handshaken JSON-RPC client. It is safe for
// concurrent use.
type Client struct {
	peer   *rpc.Peer
	router *rpc.Router
	done   chan struct{} // closed once Run has returned

	mu      sync.Mutex
	subs    map[string]*Subscription
	early   map[string][]rpc.Update
	earlyN  int
	hooked  map[string]bool // notification methods with a handler installed
	closing bool
}

// Dial connects to the agent's Unix socket at path and completes agent.hello.
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	conn, err := unixsock.Dial(ctx, socketPath)
	if err != nil {
		return nil, err
	}
	c := &Client{
		router: rpc.NewRouter(),
		done:   make(chan struct{}),
		subs:   make(map[string]*Subscription),
		early:  make(map[string][]rpc.Update),
		hooked: make(map[string]bool),
	}
	c.peer = rpc.NewPeer(conn, c.router, rpc.WithLogger(discardLogger()))
	go func() {
		defer close(c.done)
		_ = c.peer.Run(context.Background())
	}()

	hello := map[string]any{
		"protocol": api.ProtocolVersion,
		"client":   map[string]string{"name": clientName, "version": version.Version},
	}
	if err := c.Call(ctx, "agent.hello", hello, nil); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	return c, nil
}

// Call invokes method with params (nil, an object or an array) and decodes the
// result into result, which may be nil. A JSON-RPC error is returned as
// *rpc.Error.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return c.peer.Call(ctx, method, params, result)
}

// Done is closed once the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.peer.Done() }

// Close ends the connection and waits for its goroutines.
func (c *Client) Close() error {
	c.mu.Lock()
	c.closing = true
	subs := make([]*Subscription, 0, len(c.subs))
	for _, s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	err := c.peer.Close()
	<-c.done
	for _, s := range subs {
		s.stop()
	}
	return err
}

// Subscription is a live subscription created by Client.Subscribe.
type Subscription struct {
	c           *Client
	id          string
	unsubscribe string
	fn          func(seq int64, data json.RawMessage) bool

	mu         sync.Mutex
	next       uint64
	buf        map[uint64]rpc.Update
	delivering bool
	timer      *time.Timer
	stopped    bool
	done       chan struct{}
}

// ID returns the subscription id assigned by the agent.
func (s *Subscription) ID() string { return s.id }

// Done is closed when the callback asked to stop, the subscription was closed
// or the connection ended.
func (s *Subscription) Done() <-chan struct{} { return s.done }

// Close stops delivery and unsubscribes on the agent (best effort).
func (s *Subscription) Close(ctx context.Context) error {
	if !s.stop() {
		return nil
	}
	return s.c.Call(ctx, s.unsubscribe, map[string]string{"subscription": s.id}, nil)
}

// stop ends delivery; it reports whether this call did so.
func (s *Subscription) stop() bool {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return false
	}
	s.stopped = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.buf = nil
	close(s.done)
	s.mu.Unlock()
	s.c.mu.Lock()
	delete(s.c.subs, s.id)
	s.c.mu.Unlock()
	return true
}

// Subscribe calls method (a "*.subscribe" method) with params and delivers the
// resulting "notify" updates to fn in seq order, from a single goroutine at a
// time. A gap marker (dropped updates) is delivered with empty data and seq
// set to the end of the dropped range. Return false from fn to stop delivery. Updates that arrive
// before the subscribe response are buffered until the id is known.
func (c *Client) Subscribe(ctx context.Context, method string, params any, notify string, fn func(seq int64, data json.RawMessage) bool) (*Subscription, error) {
	c.hook(notify)
	var res struct {
		Subscription string `json:"subscription"`
	}
	if err := c.Call(ctx, method, params, &res); err != nil {
		return nil, err
	}
	if res.Subscription == "" {
		return nil, fmt.Errorf("%s: response has no subscription id", method)
	}
	s := &Subscription{
		c:           c,
		id:          res.Subscription,
		unsubscribe: strings.TrimSuffix(method, ".subscribe") + ".unsubscribe",
		fn:          fn,
		next:        1,
		buf:         make(map[uint64]rpc.Update),
		done:        make(chan struct{}),
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return nil, rpc.ErrClosed
	}
	c.subs[s.id] = s
	early := c.early[s.id]
	delete(c.early, s.id)
	c.earlyN -= len(early)
	c.mu.Unlock()
	for _, u := range early {
		s.push(u)
	}
	return s, nil
}

// hook installs the notification handler for a method once.
func (c *Client) hook(notify string) {
	c.mu.Lock()
	if c.hooked[notify] {
		c.mu.Unlock()
		return
	}
	c.hooked[notify] = true
	c.mu.Unlock()
	c.router.Handle(notify, func(_ context.Context, _ *rpc.Peer, params json.RawMessage) (any, error) {
		var u rpc.Update
		if err := rpc.DecodeParams(params, &u); err != nil {
			return nil, err
		}
		c.route(u)
		return nil, nil
	})
}

func (c *Client) route(u rpc.Update) {
	c.mu.Lock()
	s, ok := c.subs[u.Subscription]
	if !ok {
		if c.earlyN < maxEarlyUpdates {
			c.early[u.Subscription] = append(c.early[u.Subscription], u)
			c.earlyN++
		}
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	s.push(u)
}

// push buffers u and, unless another goroutine is already delivering, drains
// every contiguous update to the callback.
func (s *Subscription) push(u rpc.Update) {
	s.mu.Lock()
	if s.stopped || u.Seq < s.next {
		s.mu.Unlock()
		return
	}
	s.buf[u.Seq] = u
	s.drain()
}

// drain delivers buffered updates in order. It is entered with s.mu held and
// returns with it released.
func (s *Subscription) drain() {
	if s.delivering {
		s.mu.Unlock()
		return
	}
	s.delivering = true
	for !s.stopped {
		u, ok := s.buf[s.next]
		if !ok {
			break
		}
		delete(s.buf, s.next)
		s.next = u.Seq + 1
		s.mu.Unlock()
		keep := s.fn(int64(u.Seq), u.Data)
		s.mu.Lock()
		if !keep {
			s.mu.Unlock()
			s.stop()
			s.mu.Lock()
		}
	}
	s.delivering = false
	if !s.stopped && len(s.buf) > 0 && s.timer == nil {
		// A sequence number is missing: wait briefly, then skip past it.
		s.timer = time.AfterFunc(gapWait, s.skipGap)
	}
	s.mu.Unlock()
}

// skipGap moves delivery to the lowest buffered seq after a missing one.
func (s *Subscription) skipGap() {
	s.mu.Lock()
	s.timer = nil
	if s.stopped || len(s.buf) == 0 {
		s.mu.Unlock()
		return
	}
	lowest := ^uint64(0)
	for seq := range s.buf {
		lowest = min(lowest, seq)
	}
	s.next = lowest
	s.drain()
}
