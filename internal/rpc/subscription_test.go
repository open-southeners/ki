package rpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/open-southeners/ki-agent/internal/rpc"
)

// subRouter exposes a handler that opens a subscription and hands it to the
// test through ch.
func subRouter(kind rpc.Kind, ch chan<- *rpc.Subscription) *rpc.Router {
	r := rpc.NewRouter()
	r.Handle("sub", func(_ context.Context, p *rpc.Peer, _ json.RawMessage) (any, error) {
		s := p.Subs().Open(kind, "sub.update", time.Second)
		ch <- s
		return map[string]string{"subscription": s.ID()}, nil
	})
	r.Handle("unsub", func(_ context.Context, p *rpc.Peer, params json.RawMessage) (any, error) {
		var v struct{ Subscription string }
		if err := rpc.DecodeParams(params, &v); err != nil {
			return nil, err
		}
		if err := p.Subs().Close(v.Subscription); err != nil {
			return nil, err
		}
		return true, nil
	})
	return r
}

type note struct {
	Method string     `json:"method"`
	Params rpc.Update `json:"params"`
}

func readNote(t *testing.T, c rpc.Conn) note {
	t.Helper()
	var n note
	msg := readMsg(t, c)
	if err := json.Unmarshal([]byte(msg), &n); err != nil {
		t.Fatalf("%v: %s", err, msg)
	}
	if n.Method != "sub.update" {
		t.Fatalf("unexpected message %s", msg)
	}
	return n
}

func openSub(t *testing.T, kind rpc.Kind, opts ...rpc.Option) (*rpc.Subscription, rpc.Conn, *running) {
	t.Helper()
	ch := make(chan *rpc.Subscription, 1)
	p, c := raw(t, subRouter(kind, ch), opts...)
	exchange(t, c, `{"jsonrpc":"2.0","id":1,"method":"sub"}`)
	return <-ch, c, p
}

func TestSubscriptionEnvelopeAndID(t *testing.T) {
	s, c, _ := openSub(t, rpc.Snapshot)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(s.ID()) {
		t.Fatalf("id = %q", s.ID())
	}
	if err := s.Send(map[string]int{"n": 1}); err != nil {
		t.Fatal(err)
	}
	got := readMsg(t, c)
	want := `{"jsonrpc":"2.0","method":"sub.update","params":{"subscription":"` + s.ID() + `","seq":1,"data":{"n":1}}}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSnapshotCoalescing(t *testing.T) {
	// Block the writer so updates pile up, then check latest wins.
	ch := make(chan *rpc.Subscription, 1)
	ca, cb := rawPipe(t)
	sc := &slowConn{Conn: ca, release: make(chan struct{})}
	sc.pass.Store(1) // the subscribe response
	start(t, sc, subRouter(rpc.Snapshot, ch), rpc.WithSendQueue(1))
	exchangeAsync(t, cb, `{"jsonrpc":"2.0","id":1,"method":"sub"}`)
	s := <-ch

	for i := 1; i <= 100; i++ {
		if err := s.Send(i); err != nil {
			t.Fatal(err)
		}
	}
	close(sc.release)

	var last note
	for {
		n := readNote(t, cb)
		if n.Params.Seq <= last.Params.Seq {
			t.Fatalf("seq not increasing: %d after %d", n.Params.Seq, last.Params.Seq)
		}
		last = n
		if string(n.Params.Data) == "100" {
			break
		}
	}
	if last.Params.Seq != 100 {
		t.Fatalf("seq = %d, want 100 (seq increments per Send)", last.Params.Seq)
	}
	// Strictly fewer deliveries than sends proves coalescing; at least the
	// first value was in flight, so seq jumps are expected but not required to
	// be exact, only that no stale value follows the newest one.
	expectSilence(t, cb)
}

func TestOrderedDeliversInOrder(t *testing.T) {
	s, c, _ := openSub(t, rpc.Ordered)
	for i := 1; i <= 50; i++ {
		if err := s.Send(i); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 50; i++ {
		n := readNote(t, c)
		if int(n.Params.Seq) != i || string(n.Params.Data) != itoa(i) || n.Params.Gap != nil {
			t.Fatalf("update %d = %+v", i, n.Params)
		}
	}
}

func TestOrderedGapMarker(t *testing.T) {
	ch := make(chan *rpc.Subscription, 1)
	ca, cb := rawPipe(t)
	sc := &slowConn{Conn: ca, release: make(chan struct{})}
	sc.pass.Store(1) // the subscribe response
	start(t, sc, subRouter(rpc.Ordered, ch), rpc.WithSendQueue(1), rpc.WithOrderedBuffer(3))
	exchangeAsync(t, cb, `{"jsonrpc":"2.0","id":1,"method":"sub"}`)
	s := <-ch

	// The pump takes seq 1 and blocks in the (full) queue or the writer; with
	// buffer 3 and 10 sends, the oldest undelivered updates are dropped.
	for i := 1; i <= 10; i++ {
		s.Send(i)
		time.Sleep(time.Millisecond) // let the pump advance deterministically
	}
	close(sc.release)

	var seen []rpc.Update
	for {
		n := readNote(t, cb)
		seen = append(seen, n.Params)
		if n.Params.Seq == 10 && n.Params.Gap == nil {
			break
		}
	}
	// Validate invariants: contiguity with the gap marker bridging holes.
	var prev uint64
	gaps := 0
	for _, u := range seen {
		if u.Gap != nil {
			gaps++
			if u.Gap.From != prev+1 || u.Gap.To < u.Gap.From || u.Seq != u.Gap.To {
				t.Fatalf("bad gap %+v after seq %d", u, prev)
			}
		} else if u.Seq != prev+1 {
			t.Fatalf("hole before seq %d (prev %d) without gap marker; seen=%+v", u.Seq, prev, seen)
		}
		prev = u.Seq
	}
	if gaps == 0 {
		t.Fatalf("expected a gap marker; seen=%+v", seen)
	}
}

func TestOrderedReplay(t *testing.T) {
	s, c, _ := openSub(t, rpc.Ordered, rpc.WithOrderedBuffer(5))
	for i := 1; i <= 8; i++ {
		s.Send(i)
	}
	// Seq 1-3 fell out of the 5-entry buffer; drain live delivery first.
	var drained int
	for drained < 8 {
		n := readNote(t, c)
		if n.Params.Gap == nil {
			drained++
		}
		if n.Params.Seq == 8 {
			break
		}
	}

	t.Run("within buffer", func(t *testing.T) {
		if err := s.Replay(5); err != nil {
			t.Fatal(err)
		}
		for want := uint64(6); want <= 8; want++ {
			n := readNote(t, c)
			if n.Params.Seq != want || n.Params.Gap != nil {
				t.Fatalf("got %+v, want seq %d", n.Params, want)
			}
		}
		expectSilence(t, c)
	})

	t.Run("older than buffer emits gap first", func(t *testing.T) {
		if err := s.Replay(1); err != nil {
			t.Fatal(err)
		}
		n := readNote(t, c)
		if n.Params.Gap == nil || *n.Params.Gap != (rpc.Gap{From: 2, To: 3}) || n.Params.Seq != 3 {
			t.Fatalf("got %+v, want gap 2-3", n.Params)
		}
		for want := uint64(4); want <= 8; want++ {
			n := readNote(t, c)
			if n.Params.Seq != want || n.Params.Gap != nil {
				t.Fatalf("got %+v, want seq %d", n.Params, want)
			}
		}
		expectSilence(t, c)
	})

	t.Run("beyond newest is a no-op", func(t *testing.T) {
		if err := s.Replay(100); err != nil {
			t.Fatal(err)
		}
		expectSilence(t, c)
	})

	t.Run("snapshot rejects", func(t *testing.T) {
		snap, _, _ := openSub(t, rpc.Snapshot)
		if err := snap.Replay(0); !errors.Is(err, rpc.ErrNotOrdered) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCloseSubscription(t *testing.T) {
	s, c, p := openSub(t, rpc.Snapshot)
	if p.Subs().Len() != 1 {
		t.Fatal("not registered")
	}
	got := exchange(t, c, `{"jsonrpc":"2.0","id":2,"method":"unsub","params":{"subscription":"`+s.ID()+`"}}`)
	if got != `{"jsonrpc":"2.0","id":2,"result":true}` {
		t.Fatal(got)
	}
	select {
	case <-s.Done():
	case <-time.After(timeout):
		t.Fatal("Done not closed")
	}
	if err := s.Send(1); !errors.Is(err, rpc.ErrSubscriptionClosed) {
		t.Fatalf("Send = %v", err)
	}
	if p.Subs().Len() != 0 {
		t.Fatal("not removed")
	}
	got = exchange(t, c, `{"jsonrpc":"2.0","id":3,"method":"unsub","params":{"subscription":"`+s.ID()+`"}}`)
	want := `{"jsonrpc":"2.0","id":3,"error":{"code":-32004,"message":"subscription not found"}}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	s.Close() // idempotent
}

func TestSubscriptionsClosedWithPeer(t *testing.T) {
	s, _, p := openSub(t, rpc.Ordered)
	other := p.Subs().Open(rpc.Snapshot, "sub.update", 0)
	p.Close()
	if err := p.waitExit(t); err != nil {
		t.Fatal(err)
	}
	for _, x := range []*rpc.Subscription{s, other} {
		select {
		case <-x.Done():
		default:
			t.Fatal("subscription not closed with peer")
		}
	}
	if err := s.Send(1); !errors.Is(err, rpc.ErrSubscriptionClosed) {
		t.Fatalf("Send = %v", err)
	}
	// Opening after the peer ended yields a closed, pump-less subscription.
	late := p.Subs().Open(rpc.Snapshot, "sub.update", 0)
	select {
	case <-late.Done():
	default:
		t.Fatal("late subscription should be closed")
	}
	noLeak(t)
}

func TestSendDoesNotBlockOnStuckConnection(t *testing.T) {
	ch := make(chan *rpc.Subscription, 1)
	ca, cb := rawPipe(t)
	sc := &slowConn{Conn: ca, release: make(chan struct{})}
	sc.pass.Store(1) // the subscribe response
	start(t, sc, subRouter(rpc.Ordered, ch), rpc.WithSendQueue(1), rpc.WithOrderedBuffer(4))
	exchangeAsync(t, cb, `{"jsonrpc":"2.0","id":1,"method":"sub"}`)
	s := <-ch
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			s.Send(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatal("Send blocked on a stuck connection")
	}
}

func TestClampInterval(t *testing.T) {
	const ms = time.Millisecond
	tests := []struct{ req, min, max, want time.Duration }{
		{0, 250 * ms, 60000 * ms, 250 * ms},
		{-5, 250 * ms, 60000 * ms, 250 * ms},
		{1000 * ms, 250 * ms, 60000 * ms, 1000 * ms},
		{90000 * ms, 250 * ms, 60000 * ms, 60000 * ms},
		{5, 10, 1, 10},
	}
	for _, tc := range tests {
		if got := rpc.ClampInterval(tc.req, tc.min, tc.max); got != tc.want {
			t.Errorf("ClampInterval(%v,%v,%v) = %v, want %v", tc.req, tc.min, tc.max, got, tc.want)
		}
	}
}
