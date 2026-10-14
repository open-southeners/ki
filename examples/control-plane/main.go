// Command control-plane is a minimal example control plane for ki-agent.
//
// It listens for agents that dial out (control_plane.url in the agent config),
// authenticates them with a bearer token, then acts as the JSON-RPC client of
// the agent API: agent.hello, agent.info, a heartbeat subscription and,
// optionally, a container log subscription. Updates are printed as they
// arrive.
//
// It deliberately imports nothing from the ki-agent module: the protocol is
// plain JSON-RPC 2.0 over WebSocket text messages, see docs/protocol.md.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
)

// protocolVersion is the agent protocol version this control plane speaks.
// Only the major version has to match the agent's.
const protocolVersion = "1.0.0"

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	path := flag.String("path", "/agent", "HTTP path agents connect to")
	token := flag.String("token", "", "bearer token agents must present (required)")
	logs := flag.String("logs", "", "container id or name whose logs to follow")
	flag.Parse()
	if *token == "" {
		log.Fatal("-token is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var sessions sync.WaitGroup
	mux := http.NewServeMux()
	mux.HandleFunc(*path, func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(*token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return // Accept already replied
		}
		c.SetReadLimit(4 << 20)
		sessions.Add(1)
		defer sessions.Done()
		lg := log.New(os.Stdout, "["+r.RemoteAddr+"] ", log.LstdFlags|log.Lmicroseconds)
		lg.Printf("connected (%s, protocol %s)", r.UserAgent(), r.Header.Get("Ki-Protocol"))
		runSession(ctx, c, lg, *logs)
		lg.Printf("disconnected")
	})

	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		// Hijacked WebSocket connections are not tracked by Shutdown; the
		// sessions notice ctx themselves and close their connections.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Printf("control plane listening on ws://%s%s", *addr, *path)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	sessions.Wait()
}

// runSession drives one agent connection until it ends or ctx is cancelled.
func runSession(ctx context.Context, ws *websocket.Conn, lg *log.Logger, logsID string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := newClient(ws, lg)
	go func() {
		c.readLoop(ctx)
		cancel()
	}()
	go func() {
		<-ctx.Done()
		_ = ws.Close(websocket.StatusNormalClosure, "bye")
	}()

	// Pipeline agent.hello and agent.info: the agent handles requests in
	// order until the handshake completes, so no need to wait in between.
	hello := c.start("agent.hello", map[string]any{
		"protocol": protocolVersion,
		"client":   map[string]string{"name": "example-control-plane", "version": "0.1.0"},
	})
	info := c.start("agent.info", nil)
	res, err := hello.wait(ctx)
	if err != nil {
		lg.Printf("agent.hello failed: %v", err) // e.g. -32003 incompatible version
		return
	}
	lg.Printf("hello: %s", res)
	if res, err = info.wait(ctx); err != nil {
		lg.Printf("agent.info failed: %v", err)
		return
	}
	lg.Printf("info: %s", res)

	sub, err := c.call(ctx, "agent.heartbeat.subscribe", map[string]any{"interval_ms": 2000})
	if err != nil {
		lg.Printf("heartbeat subscribe failed: %v", err)
		return
	}
	c.watch(sub, "heartbeat")

	if logsID != "" {
		sub, err = c.call(ctx, "containers.logs.subscribe", map[string]any{
			"id": logsID, "tail": 20, "timestamps": true,
		})
		if err != nil {
			lg.Printf("logs subscribe failed: %v", err) // -32006 not found, -32008 no engine
		} else {
			c.watch(sub, "logs "+logsID)
		}
	}
	<-ctx.Done()
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("error %d: %s", e.Code, e.Message) }

// message is any inbound JSON-RPC message: a response (has id) or a
// notification (has method). The agent never sends requests.
type message struct {
	ID     *int64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// update is the params object of every subscription notification.
type update struct {
	Subscription string          `json:"subscription"`
	Seq          uint64          `json:"seq"`
	Data         json.RawMessage `json:"data"`
	Gap          *struct {
		From, To uint64
	} `json:"gap"`
}

type client struct {
	ws *websocket.Conn
	lg *log.Logger

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan message
	subs    map[string]*stream
	// early holds updates that overtook the response carrying their
	// subscription id (the agent may send the first update before the reply).
	early map[string][]update
}

// stream remembers the label and last printed seq of one subscription.
type stream struct {
	label string
	last  uint64
}

func newClient(ws *websocket.Conn, lg *log.Logger) *client {
	return &client{
		ws: ws, lg: lg,
		pending: map[int64]chan message{},
		subs:    map[string]*stream{},
		early:   map[string][]update{},
	}
}

// pendingCall is a request that has been sent but not yet answered.
type pendingCall struct {
	ch  chan message
	err error
}

func (p *pendingCall) wait(ctx context.Context) (json.RawMessage, error) {
	if p.err != nil {
		return nil, p.err
	}
	select {
	case m, ok := <-p.ch:
		if !ok {
			return nil, errors.New("connection closed")
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// start sends a request and returns immediately.
func (c *client) start(method string, params any) *pendingCall {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan message, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return &pendingCall{err: err}
	}
	return &pendingCall{ch: ch}
}

// call sends a request and waits for its result, returning the subscription
// id if the result carries one (and the raw result otherwise).
func (c *client) call(ctx context.Context, method string, params any) (string, error) {
	res, err := c.start(method, params).wait(ctx)
	if err != nil {
		return "", err
	}
	var r struct {
		Subscription string `json:"subscription"`
	}
	_ = json.Unmarshal(res, &r)
	return r.Subscription, nil
}

// watch starts printing the updates of a subscription, including any that
// already arrived.
func (c *client) watch(id, label string) {
	c.mu.Lock()
	c.subs[id] = &stream{label: label}
	early := c.early[id]
	delete(c.early, id)
	c.mu.Unlock()
	for _, u := range early {
		c.print(u)
	}
}

func (c *client) readLoop(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.mu.Unlock()
	}()
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		var m message
		if err := json.Unmarshal(data, &m); err != nil {
			c.lg.Printf("ignoring malformed message: %v", err)
			continue
		}
		switch {
		case m.ID != nil:
			c.mu.Lock()
			ch := c.pending[*m.ID]
			delete(c.pending, *m.ID)
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		case strings.HasSuffix(m.Method, ".update"):
			var u update
			if json.Unmarshal(m.Params, &u) == nil {
				c.deliver(m.Method, u)
			}
		default:
			c.lg.Printf("ignoring %q", m.Method)
		}
	}
}

func (c *client) deliver(method string, u update) {
	c.mu.Lock()
	_, known := c.subs[u.Subscription]
	if !known {
		c.early[u.Subscription] = append(c.early[u.Subscription], u)
	}
	c.mu.Unlock()
	if known {
		c.print(u)
	}
}

// print shows one update. Updates of a subscription are delivered in order
// over the single connection; seq is checked anyway, since it is the
// authority: anything at or below the last seq is stale and dropped.
func (c *client) print(u update) {
	c.mu.Lock()
	s := c.subs[u.Subscription]
	stale := u.Seq <= s.last
	if !stale {
		s.last = u.Seq
	}
	c.mu.Unlock()
	switch {
	case stale:
		c.lg.Printf("%s: dropping stale seq %d", s.label, u.Seq)
	case u.Gap != nil:
		// seq == gap.to: lines from..to were dropped by a slow connection.
		c.lg.Printf("%s #%d: GAP, missed seq %d..%d", s.label, u.Seq, u.Gap.From, u.Gap.To)
	default:
		c.lg.Printf("%s #%d: %s", s.label, u.Seq, u.Data)
	}
}
