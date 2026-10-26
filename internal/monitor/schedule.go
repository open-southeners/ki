package monitor

import (
	"sync"
	"time"
)

// schedule tracks the active subscribers of one sampler and derives its
// sampling interval: the configured default while nobody is subscribed, the
// smallest subscriber interval (never above the default) otherwise.
type schedule struct {
	def      time.Duration
	min, max time.Duration // bounds of a subscriber's interval

	mu   sync.Mutex
	next uint64
	subs map[uint64]subscriber
	wake chan struct{}
}

type subscriber struct {
	interval time.Duration
	perCore  bool
}

func newSchedule(def, min, max time.Duration) *schedule {
	return &schedule{def: def, min: min, max: max, subs: make(map[uint64]subscriber), wake: make(chan struct{}, 1)}
}

// clamp limits a requested subscriber interval to the schedule's bounds.
func (s *schedule) clamp(req time.Duration) time.Duration {
	if req > s.max {
		req = s.max
	}
	if req < s.min {
		req = s.min
	}
	return req
}

// add registers a subscriber and wakes the sampler so a faster interval takes
// effect immediately.
func (s *schedule) add(interval time.Duration, perCore bool) uint64 {
	s.mu.Lock()
	s.next++
	id := s.next
	s.subs[id] = subscriber{interval: interval, perCore: perCore}
	s.mu.Unlock()
	s.poke()
	return id
}

func (s *schedule) remove(id uint64) {
	s.mu.Lock()
	delete(s.subs, id)
	s.mu.Unlock()
	s.poke()
}

func (s *schedule) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// interval returns the current sampling interval.
func (s *schedule) interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.def
	for _, sub := range s.subs {
		if sub.interval < d {
			d = sub.interval
		}
	}
	return d
}

func (s *schedule) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs)
}

func (s *schedule) anyPerCore() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range s.subs {
		if sub.perCore {
			return true
		}
	}
	return false
}

// notifier wakes waiters each time a sampler publishes a new sample.
type notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

func newNotifier() *notifier { return &notifier{ch: make(chan struct{})} }

// wait returns a channel closed by the next fire.
func (n *notifier) wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ch
}

func (n *notifier) fire() {
	n.mu.Lock()
	close(n.ch)
	n.ch = make(chan struct{})
	n.mu.Unlock()
}
