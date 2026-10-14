package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
)

const (
	defaultSendQueue  = 256
	defaultRingBuffer = 1024
)

// Option configures a Peer.
type Option func(*Peer)

// WithSendQueue sets the capacity of the outbound message queue (default
// 256). Values below 1 are ignored.
func WithSendQueue(n int) Option {
	return func(p *Peer) {
		if n >= 1 {
			p.queueSize = n
		}
	}
}

// WithOrderedBuffer sets the per-subscription ring buffer capacity used by
// Ordered subscriptions (default 1024). Values below 1 are ignored.
func WithOrderedBuffer(n int) Option {
	return func(p *Peer) {
		if n >= 1 {
			p.ringSize = n
		}
	}
}

// WithLogger sets the logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(p *Peer) {
		if l != nil {
			p.log = l
		}
	}
}

// WithSerialDispatch makes the read loop handle each incoming request
// synchronously, in arrival order, for as long as serial(p) returns true; once
// it returns false dispatch is concurrent again. It is evaluated before every
// payload, so a handler that flips the state (for example a successful
// handshake) takes effect for the very next request. While serial, a handler
// must not block on a Call to its own peer, as the read loop is busy.
func WithSerialDispatch(serial func(p *Peer) bool) Option {
	return func(p *Peer) { p.serial = serial }
}

// Peer is one end of a JSON-RPC 2.0 connection. It is symmetric: it serves
// incoming requests through its Router and can issue its own Calls and
// Notifications over the same Conn. All methods are safe for concurrent use.
//
// A Peer owns a single writer goroutine, so the Conn sees one writer, and
// the Run loop is the single reader.
type Peer struct {
	conn   Conn
	router *Router
	log    *slog.Logger

	serial     func(*Peer) bool
	closeAfter atomic.Bool

	queueSize int
	ringSize  int

	ctx    context.Context
	cancel context.CancelFunc
	out    chan []byte

	started  atomic.Bool
	once     sync.Once
	cause    error // why the peer ended; valid once ctx is done
	handlers sync.WaitGroup

	nextID  atomic.Uint64
	pmu     sync.Mutex
	pending map[uint64]chan *Response

	smu   sync.RWMutex
	state map[string]any

	subs *Subscriptions
}

// NewPeer creates a Peer over conn serving requests with r (which may be nil
// for a client-only peer). No goroutines run until Run is called.
func NewPeer(conn Conn, r *Router, opts ...Option) *Peer {
	p := &Peer{
		conn:      conn,
		router:    r,
		log:       slog.Default(),
		queueSize: defaultSendQueue,
		ringSize:  defaultRingBuffer,
		pending:   make(map[uint64]chan *Response),
		state:     make(map[string]any),
	}
	for _, o := range opts {
		o(p)
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.out = make(chan []byte, p.queueSize)
	p.subs = newSubscriptions(p)
	return p
}

// Subs returns the peer's subscription manager.
func (p *Peer) Subs() *Subscriptions { return p.subs }

// Set stores per-peer state under key (for example handshake status).
func (p *Peer) Set(key string, val any) {
	p.smu.Lock()
	p.state[key] = val
	p.smu.Unlock()
}

// Get returns the per-peer state stored under key.
func (p *Peer) Get(key string) (any, bool) {
	p.smu.RLock()
	v, ok := p.state[key]
	p.smu.RUnlock()
	return v, ok
}

// Done returns a channel closed once the peer has been closed, either by
// Close, by ctx cancellation of Run, or because the connection failed.
func (p *Peer) Done() <-chan struct{} { return p.ctx.Done() }

// Close shuts the peer down: pending Calls fail with ErrClosed, subscriptions
// are closed and the Conn is closed. It does not wait for goroutines to exit
// (Run does) so it is safe to call from a handler. Close is idempotent and
// returns the error from closing the Conn on the first call.
func (p *Peer) Close() error { return p.shutdown(nil) }

func (p *Peer) shutdown(cause error) (err error) {
	p.once.Do(func() {
		p.cause = cause
		p.cancel()
		err = p.conn.Close()
		p.subs.closeAll()
	})
	return err
}

// Run reads from the connection until it fails, the peer is closed or ctx is
// cancelled, dispatching each incoming request on its own goroutine. It
// returns only after every goroutine started by the peer (handlers, writer,
// subscription pumps) has exited. The result is nil after Close or a clean
// remote close (io.EOF), ctx.Err() if ctx ended the run, and the Conn error
// otherwise. Run may be called once.
func (p *Peer) Run(ctx context.Context) error {
	if !p.started.CompareAndSwap(false, true) {
		return errors.New("rpc: Run called more than once")
	}
	stop := context.AfterFunc(ctx, func() { p.shutdown(ctx.Err()) })
	defer stop()

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		p.writeLoop()
	}()

	for p.ctx.Err() == nil {
		data, err := p.conn.Read(p.ctx)
		if err != nil {
			p.shutdown(err)
			break
		}
		p.dispatch(data)
	}

	<-p.ctx.Done()
	p.handlers.Wait()
	p.subs.wait()
	<-writerDone

	if errors.Is(p.cause, io.EOF) {
		return nil
	}
	return p.cause
}

func (p *Peer) writeLoop() {
	for {
		select {
		case <-p.ctx.Done():
			return
		case msg := <-p.out:
			if msg == nil { // close-after-response sentinel: all earlier writes are done
				p.shutdown(nil)
				return
			}
			if err := p.conn.Write(p.ctx, msg); err != nil {
				p.shutdown(err)
				return
			}
		}
	}
}

// enqueue queues msg for the writer, blocking while the queue is full until
// ctx or the peer ends.
func (p *Peer) enqueue(ctx context.Context, msg []byte) error {
	if p.ctx.Err() != nil {
		return ErrClosed
	}
	select {
	case p.out <- msg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	}
}

// CloseAfterResponse makes the peer close its connection once the response to
// the request currently being handled has been written. Call it from a
// handler; the response is flushed before the connection is closed.
func (p *Peer) CloseAfterResponse() { p.closeAfter.Store(true) }

// reply sends a handler's response and, if requested, queues the close
// sentinel behind it.
func (p *Peer) reply(v any) {
	if err := p.send(p.ctx, v); err != nil {
		p.log.Debug("rpc: send response", "err", err)
		return
	}
	if p.closeAfter.Load() {
		_ = p.enqueue(p.ctx, nil)
	}
}

func (p *Peer) send(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("rpc: encode message: %w", err)
	}
	return p.enqueue(ctx, b)
}

// marshalParams encodes params, which must be nil, an object or an array.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("rpc: encode params: %w", err)
	}
	b = bytes.TrimSpace(b)
	switch {
	case string(b) == "null":
		return nil, nil
	case len(b) > 0 && (b[0] == '{' || b[0] == '['):
		return b, nil
	}
	return nil, errors.New("rpc: params must encode to a JSON object or array")
}

// Call sends a request and waits for its response, decoding the result into
// result (which may be nil to discard it). A JSON-RPC error response is
// returned as *Error. If ctx ends first Call returns ctx.Err(); if the peer
// closes it returns ErrClosed. Run must be running for the response to arrive.
func (p *Peer) Call(ctx context.Context, method string, params, result any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	id := p.nextID.Add(1)
	ch := make(chan *Response, 1)

	p.pmu.Lock()
	if p.ctx.Err() != nil {
		p.pmu.Unlock()
		return ErrClosed
	}
	p.pending[id] = ch
	p.pmu.Unlock()
	defer func() {
		p.pmu.Lock()
		delete(p.pending, id)
		p.pmu.Unlock()
	}()

	req := Request{Method: method, Params: raw, ID: json.RawMessage(fmt.Sprint(id))}
	if err := p.send(ctx, req); err != nil {
		return err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("rpc: decode result of %q: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return ErrClosed
	}
}

// Notify sends a notification (a request without id) and does not wait for
// any reply.
func (p *Peer) Notify(ctx context.Context, method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return p.send(ctx, Request{Method: method, Params: raw})
}

// deliver routes a response to the Call waiting for it.
func (p *Peer) deliver(resp *Response) {
	id, ok := callID(resp.ID)
	if ok {
		p.pmu.Lock()
		ch, found := p.pending[id]
		delete(p.pending, id)
		p.pmu.Unlock()
		if found {
			ch <- resp // buffered, one send per id
			return
		}
	}
	p.log.Warn("rpc: dropping response with unknown id", "id", string(resp.ID))
}

// dispatch handles one inbound payload without blocking the read loop.
func (p *Peer) dispatch(data []byte) {
	items, batch := parsePayload(data)
	serial := p.serial != nil && p.serial(p)
	run := func(f func()) {
		if serial {
			f()
			return
		}
		p.handlers.Add(1)
		go func() {
			defer p.handlers.Done()
			f()
		}()
	}
	if !batch {
		it := items[0]
		switch {
		case it.resp != nil:
			p.deliver(it.resp)
		case it.invalid != nil:
			if err := p.send(p.ctx, it.invalid); err != nil {
				p.log.Debug("rpc: send error response", "err", err)
			}
		default:
			run(func() {
				if resp := p.serve(it.req); resp != nil {
					p.reply(resp)
				}
			})
		}
		return
	}

	run(func() {
		results := make([]*Response, len(items))
		var wg sync.WaitGroup
		for i, it := range items {
			switch {
			case it.resp != nil:
				p.deliver(it.resp)
			case it.invalid != nil:
				results[i] = it.invalid
			default:
				wg.Add(1)
				go func() {
					defer wg.Done()
					results[i] = p.serve(it.req)
				}()
			}
		}
		wg.Wait()
		out := make([]*Response, 0, len(results))
		for _, r := range results {
			if r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return // all notifications: no response at all
		}
		p.reply(out)
	})
}

// serve runs the handler for req and returns the response, or nil for a
// notification.
func (p *Peer) serve(req *Request) *Response {
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()

	result, err := p.invoke(ctx, req)
	if req.IsNotification() {
		if err != nil {
			p.log.Warn("rpc: notification handler failed", "method", req.Method, "err", err)
		}
		return nil
	}
	resp := &Response{ID: req.ID}
	if err != nil {
		resp.Error = p.toError(req.Method, err)
		return resp
	}
	b, merr := json.Marshal(result)
	if merr != nil {
		p.log.Error("rpc: encode result", "method", req.Method, "err", merr)
		resp.Error = NewError(CodeInternalError, "internal error")
		return resp
	}
	resp.Result = b
	return resp
}

func (p *Peer) invoke(ctx context.Context, req *Request) (result any, err error) {
	h, ok := p.router.lookup(req.Method)
	if !ok {
		return nil, Errorf(CodeMethodNotFound, "method not found: %s", req.Method)
	}
	defer func() {
		if r := recover(); r != nil {
			p.log.Error("rpc: handler panic", "method", req.Method, "panic", r, "stack", string(debug.Stack()))
			result, err = nil, NewError(CodeInternalError, "internal error")
		}
	}()
	return h(ctx, p, req.Params)
}

func (p *Peer) toError(method string, err error) *Error {
	var re *Error
	if errors.As(err, &re) {
		return re
	}
	p.log.Error("rpc: handler error", "method", method, "err", err)
	return NewError(CodeInternalError, "internal error")
}
