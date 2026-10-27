package deploys

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/open-southeners/ki/internal/rpc"
	"github.com/open-southeners/ki/internal/store"
)

// truncatedLine is the one agent line that marks the end of the stored log.
const truncatedLine = "log truncated: further output is only streamed live"

// broadcaster persists a deploy's log lines and fans them out to the
// subscriptions attached to it. Everything happens under one mutex, so a
// subscription attached with a replay sees every line exactly once.
type broadcaster struct {
	st     *store.Store
	log    *slog.Logger
	id     int64
	maxLog int

	mu        sync.Mutex
	seq       int64
	bytes     int
	truncated bool
	subs      []*rpc.Subscription
	final     *Deploy // set once the deploy finished
}

func newBroadcaster(st *store.Store, log *slog.Logger, id int64, maxLog int) *broadcaster {
	return &broadcaster{st: st, log: log, id: id, maxLog: maxLog}
}

// line records and streams one log line.
func (b *broadcaster) line(stream, text string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.final != nil {
		return
	}
	if !b.truncated {
		if b.bytes+len(text) > b.maxLog {
			b.truncated = true
			seq := b.persist("agent", truncatedLine)
			b.send(Update{Type: "log", Seq: seq, Stream: "agent", Text: truncatedLine})
		} else {
			b.bytes += len(text)
			b.send(Update{Type: "log", Seq: b.persist(stream, text), Stream: stream, Text: text})
			return
		}
	}
	b.send(Update{Type: "log", Stream: stream, Text: text})
}

// persist stores one line under the next seq. The caller holds b.mu.
func (b *broadcaster) persist(stream, text string) int64 {
	b.seq++
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.st.AppendDeployLog(ctx, b.id, b.seq, stream, text); err != nil {
		b.log.Warn("persist deploy log", "deploy", b.id, "err", err)
	}
	return b.seq
}

// send delivers u to every attached subscription, dropping closed ones.
// The caller holds b.mu.
func (b *broadcaster) send(u Update) {
	live := b.subs[:0]
	for _, s := range b.subs {
		if s.Send(u) == nil {
			live = append(live, s)
		}
	}
	for i := len(live); i < len(b.subs); i++ {
		b.subs[i] = nil
	}
	b.subs = live
}

// finish pushes the final status to every subscription and closes them. Later
// attaches replay the log and get the same status.
func (b *broadcaster) finish(d Deploy) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.final != nil {
		return
	}
	b.final = &d
	b.send(Update{Type: "status", Deploy: &d})
	for _, s := range b.subs {
		s.CloseAfterDrain()
	}
	b.subs = nil
}

// attach replays the stored log after sinceSeq into sub and then attaches it
// to the live stream (or, for a finished deploy, sends the final status and
// closes it).
func (b *broadcaster) attach(ctx context.Context, sub *rpc.Subscription, sinceSeq int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := replayLog(ctx, b.st, sub, b.id, sinceSeq); err != nil {
		return err
	}
	if b.final != nil {
		_ = sub.Send(Update{Type: "status", Deploy: b.final})
		sub.CloseAfterDrain()
		return nil
	}
	b.subs = append(b.subs, sub)
	return nil
}

// replayLog sends the stored lines of deploy id after sinceSeq to sub.
func replayLog(ctx context.Context, st *store.Store, sub *rpc.Subscription, id, sinceSeq int64) error {
	lines, err := st.DeployLogs(ctx, id, sinceSeq)
	if err != nil {
		return err
	}
	for _, l := range lines {
		if err := sub.Send(Update{Type: "log", Seq: l.Seq, Stream: l.Stream, Text: l.Text}); err != nil {
			return err
		}
	}
	return nil
}
