package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
)

// Handler serves one JSON-RPC method. The returned value is marshalled as the
// response result. Returning a *Error (possibly wrapped) sends that error to
// the caller; any other error becomes a generic InternalError and is logged.
// For notifications the result and error are discarded (errors are logged).
//
// ctx is cancelled when the peer closes.
type Handler func(ctx context.Context, p *Peer, params json.RawMessage) (any, error)

// Middleware wraps the Handler registered for method, typically to gate or
// instrument calls. It is invoked per call, so it may inspect method freely.
type Middleware func(method string, next Handler) Handler

// Router maps method names to handlers. It is safe for concurrent use and may
// be shared by many Peers.
type Router struct {
	mu       sync.RWMutex
	handlers map[string]Handler
	mw       []Middleware
}

// NewRouter returns an empty Router.
func NewRouter() *Router {
	return &Router{handlers: make(map[string]Handler)}
}

// Handle registers h for method. It panics if method is empty, h is nil or
// method is already registered, as these are programming errors.
func (r *Router) Handle(method string, h Handler) {
	if method == "" || h == nil {
		panic("rpc: Handle requires a method name and a handler")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.handlers[method]; dup {
		panic(fmt.Sprintf("rpc: method %q registered twice", method))
	}
	r.handlers[method] = h
}

// Use appends middleware. The first middleware added is the outermost. It
// applies to every registered method, including ones registered later, but
// not to unknown methods (which yield MethodNotFound).
func (r *Router) Use(mw Middleware) {
	if mw == nil {
		panic("rpc: Use requires a middleware")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mw = append(r.mw, mw)
}

// Methods returns the registered method names in sorted order.
func (r *Router) Methods() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.handlers))
	for n := range r.handlers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// lookup returns the handler for method with middleware applied.
func (r *Router) lookup(method string) (Handler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.handlers[method]
	if !ok {
		return nil, false
	}
	for i := len(r.mw) - 1; i >= 0; i-- {
		h = r.mw[i](method, h)
	}
	return h, true
}
