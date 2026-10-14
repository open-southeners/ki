package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Kind selects a subscription's delivery semantics.
type Kind int

const (
	// Snapshot subscriptions carry the latest state: when updates cannot be
	// delivered as fast as they are produced, older undelivered values are
	// replaced by the newest (latest wins). Sequence numbers still advance for
	// every Send, so consumers see gaps in seq but never stale data.
	Snapshot Kind = iota + 1
	// Ordered subscriptions carry an event stream. Updates are buffered in a
	// per-subscription ring buffer; if it overflows the oldest are dropped and
	// delivery resumes with a gap marker. The buffer also serves Replay.
	Ordered
)

// ErrSubscriptionClosed is returned by Send on a closed subscription.
var ErrSubscriptionClosed = errors.New("rpc: subscription closed")

// ErrNotOrdered is returned by Replay on a non-Ordered subscription.
var ErrNotOrdered = errors.New("rpc: replay requires an ordered subscription")

// Gap describes a range of sequence numbers (inclusive) that were dropped
// before delivery.
type Gap struct {
	From uint64 `json:"from"`
	To   uint64 `json:"to"`
}

// Update is the params object of every subscription notification. Exactly one
// of Data and Gap is present. For a gap marker, Seq equals Gap.To so that the
// sequence a consumer has seen stays contiguous with the next update.
type Update struct {
	Subscription string          `json:"subscription"`
	Seq          uint64          `json:"seq"`
	Data         json.RawMessage `json:"data,omitempty"`
	Gap          *Gap            `json:"gap,omitempty"`
}

// ClampInterval limits a client-requested interval to [min, max]. If max is
// below min, min wins.
func ClampInterval(req, min, max time.Duration) time.Duration {
	if req > max {
		req = max
	}
	if req < min {
		req = min
	}
	return req
}

// Subscriptions manages the subscriptions of one Peer. All subscriptions are
// closed when the peer ends.
type Subscriptions struct {
	peer *Peer

	mu     sync.Mutex
	subs   map[string]*Subscription
	closed bool
	wg     sync.WaitGroup
}

func newSubscriptions(p *Peer) *Subscriptions {
	return &Subscriptions{peer: p, subs: make(map[string]*Subscription)}
}

// Open creates a subscription that delivers updates as notifications named
// notifyMethod. interval is the producer's update period; it is recorded for
// the producer's use (see Subscription.Interval) and not enforced. On a peer
// that has already ended, the returned subscription is already closed.
func (m *Subscriptions) Open(kind Kind, notifyMethod string, interval time.Duration) *Subscription {
	ctx, cancel := context.WithCancel(m.peer.ctx)
	s := &Subscription{
		id:       newSubscriptionID(),
		kind:     kind,
		method:   notifyMethod,
		interval: interval,
		mgr:      m,
		ctx:      ctx,
		cancel:   cancel,
		wake:     make(chan struct{}, 1),
		deliver:  1,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		cancel()
		return s
	}
	m.subs[s.id] = s
	m.wg.Add(1)
	go s.pump()
	return s
}

// Get returns the open subscription with the given id.
func (m *Subscriptions) Get(id string) (*Subscription, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.subs[id]
	return s, ok
}

// Close closes the subscription with the given id, or returns a
// SubscriptionNotFound *Error if there is none.
func (m *Subscriptions) Close(id string) error {
	s, ok := m.Get(id)
	if !ok {
		return NewError(CodeSubscriptionNotFound, "subscription not found")
	}
	s.Close()
	return nil
}

// Len returns the number of open subscriptions.
func (m *Subscriptions) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.subs)
}

func (m *Subscriptions) closeAll() {
	m.mu.Lock()
	m.closed = true
	all := make([]*Subscription, 0, len(m.subs))
	for _, s := range m.subs {
		all = append(all, s)
	}
	m.subs = make(map[string]*Subscription)
	m.mu.Unlock()
	for _, s := range all {
		s.cancel()
	}
}

// wait blocks until every subscription pump has exited. Call only after
// closeAll.
func (m *Subscriptions) wait() { m.wg.Wait() }

func (m *Subscriptions) remove(s *Subscription) {
	m.mu.Lock()
	delete(m.subs, s.id)
	m.mu.Unlock()
}

type entry struct {
	seq  uint64
	data json.RawMessage
}

// Subscription is a server-side stream of notifications to the peer. It is
// safe for concurrent use. A dedicated goroutine delivers updates so Send
// never blocks on a slow connection.
type Subscription struct {
	id       string
	kind     Kind
	method   string
	interval time.Duration
	mgr      *Subscriptions
	ctx      context.Context
	cancel   context.CancelFunc
	wake     chan struct{}

	mu  sync.Mutex
	seq uint64 // last assigned sequence number

	// Snapshot state.
	pending    json.RawMessage
	pendingSeq uint64
	hasPending bool

	// Ordered state: ring holds the most recent entries (contiguous seqs);
	// deliver is the next seq to emit and may lag behind or be rewound by Replay.
	ring    []entry
	deliver uint64
}

func newSubscriptionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("rpc: crypto/rand: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// ID returns the subscription's identifier (128 random bits, hex encoded).
func (s *Subscription) ID() string { return s.id }

// Kind returns the subscription's delivery semantics.
func (s *Subscription) Kind() Kind { return s.kind }

// Interval returns the interval given to Open.
func (s *Subscription) Interval() time.Duration { return s.interval }

// Done returns a channel closed when the subscription is closed, explicitly or
// because the peer ended. Producers should stop when it closes.
func (s *Subscription) Done() <-chan struct{} { return s.ctx.Done() }

// Close ends the subscription. It is idempotent.
func (s *Subscription) Close() {
	s.cancel()
	s.mgr.remove(s)
}

// Send assigns the next sequence number (starting at 1) to payload and queues
// it for delivery. It never blocks on the connection.
func (s *Subscription) Send(payload any) error {
	data, err := marshalRaw(payload)
	if err != nil {
		return fmt.Errorf("rpc: encode subscription payload: %w", err)
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return ErrSubscriptionClosed
	}
	s.seq++
	switch s.kind {
	case Ordered:
		if len(s.ring) >= s.mgr.peer.ringSize {
			s.ring[0] = entry{}
			s.ring = s.ring[1:]
		}
		s.ring = append(s.ring, entry{s.seq, data})
	default:
		s.pending, s.pendingSeq, s.hasPending = data, s.seq, true
	}
	s.mu.Unlock()
	s.signal()
	return nil
}

// Replay re-queues, for an Ordered subscription, every buffered update with a
// sequence number greater than sinceSeq, ahead of new ones. If sinceSeq is
// older than the oldest buffered update a gap marker covering the missing
// range is delivered first. Use sinceSeq 0 to replay everything buffered.
func (s *Subscription) Replay(sinceSeq uint64) error {
	if s.kind != Ordered {
		return ErrNotOrdered
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return ErrSubscriptionClosed
	}
	target := sinceSeq + 1
	if sinceSeq >= s.seq {
		target = s.seq + 1
	}
	if target < s.deliver {
		s.deliver = target
	}
	s.mu.Unlock()
	s.signal()
	return nil
}

func marshalRaw(v any) (json.RawMessage, error) {
	if r, ok := v.(json.RawMessage); ok && len(r) > 0 {
		return append(json.RawMessage(nil), r...), nil
	}
	return json.Marshal(v)
}

func (s *Subscription) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// next returns the next update to emit, if any.
func (s *Subscription) next() (Update, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := Update{Subscription: s.id}
	if s.kind != Ordered {
		if !s.hasPending {
			return u, false
		}
		u.Seq, u.Data = s.pendingSeq, s.pending
		s.pending, s.hasPending = nil, false
		return u, true
	}
	if s.deliver > s.seq || len(s.ring) == 0 {
		return u, false
	}
	first := s.ring[0].seq
	if s.deliver < first {
		u.Gap = &Gap{From: s.deliver, To: first - 1}
		u.Seq = first - 1
		s.deliver = first
		return u, true
	}
	e := s.ring[s.deliver-first]
	u.Seq, u.Data = e.seq, e.data
	s.deliver++
	return u, true
}

// pump is the subscription's delivery goroutine.
func (s *Subscription) pump() {
	defer s.mgr.wg.Done()
	for {
		select {
		case <-s.wake:
		case <-s.ctx.Done():
			return
		}
		for {
			u, ok := s.next()
			if !ok {
				break
			}
			err := s.mgr.peer.send(s.ctx, Request{Method: s.method, Params: mustMarshal(u)})
			if err != nil {
				if s.ctx.Err() == nil {
					s.mgr.peer.log.Error("rpc: subscription send", "subscription", s.id, "err", err)
				}
				return
			}
		}
	}
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // Update only holds RawMessage values that were validated
	}
	return b
}
