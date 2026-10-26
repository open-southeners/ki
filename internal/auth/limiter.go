package auth

import (
	"container/list"
	"sync"
	"time"
)

// Default IPLimiter settings.
const (
	DefaultIPLimit  = 10
	DefaultIPWindow = 15 * time.Minute
	maxLimiterKeys  = 10000
)

// IPLimiter is a sliding-window failure limiter keyed by client address. It is
// safe for concurrent use. Time is supplied by callers, so tests control the clock.
type IPLimiter struct {
	limit   int
	window  time.Duration
	maxKeys int

	mu  sync.Mutex
	m   map[string]*list.Element // value: *limiterEntry
	lru *list.List               // front = most recently touched
}

type limiterEntry struct {
	key   string
	fails []time.Time // ascending
}

// NewIPLimiter returns a limiter allowing limit failures per window per key.
func NewIPLimiter(limit int, window time.Duration) *IPLimiter {
	return &IPLimiter{limit: limit, window: window, maxKeys: maxLimiterKeys,
		m: map[string]*list.Element{}, lru: list.New()}
}

// NewDefaultIPLimiter returns a limiter with 10 failures per 15 minutes.
func NewDefaultIPLimiter() *IPLimiter { return NewIPLimiter(DefaultIPLimit, DefaultIPWindow) }

func (l *IPLimiter) prune(e *limiterEntry, now time.Time) {
	cut := now.Add(-l.window)
	i := 0
	for i < len(e.fails) && !e.fails[i].After(cut) {
		i++
	}
	e.fails = e.fails[i:]
}

// Allow reports whether key may attempt now. When it may not, retryAfter is
// how long until the failure that blocks it leaves the window.
func (l *IPLimiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, found := l.m[key]
	if !found {
		return true, 0
	}
	e := el.Value.(*limiterEntry)
	l.prune(e, now)
	if len(e.fails) == 0 {
		l.remove(el)
		return true, 0
	}
	if len(e.fails) < l.limit {
		return true, 0
	}
	return false, e.fails[len(e.fails)-l.limit].Add(l.window).Sub(now)
}

// Fail records a failure for key at now.
func (l *IPLimiter) Fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, found := l.m[key]
	if !found {
		el = l.lru.PushFront(&limiterEntry{key: key})
		l.m[key] = el
		for len(l.m) > l.maxKeys {
			l.remove(l.lru.Back())
		}
	} else {
		l.lru.MoveToFront(el)
	}
	e := el.Value.(*limiterEntry)
	l.prune(e, now)
	e.fails = append(e.fails, now)
	if len(e.fails) > l.limit { // older entries can never matter again
		e.fails = e.fails[len(e.fails)-l.limit:]
	}
}

// Reset forgets all failures for key.
func (l *IPLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.m[key]; ok {
		l.remove(el)
	}
}

func (l *IPLimiter) remove(el *list.Element) {
	delete(l.m, el.Value.(*limiterEntry).key)
	l.lru.Remove(el)
}

// Account lockout settings.
const (
	lockAfter = 5
	lockBase  = 15 * time.Minute
	lockMax   = 24 * time.Hour
)

// NextLock returns the time until which an account stays locked after
// consecutiveFailures failed logins, or the zero Time if it is not locked yet.
// The 5th failure locks for 15 minutes; each further failure doubles it, capped
// at 24 hours.
func NextLock(consecutiveFailures int, now time.Time) time.Time {
	if consecutiveFailures < lockAfter {
		return time.Time{}
	}
	d := lockBase
	for i := lockAfter; i < consecutiveFailures && d < lockMax; i++ {
		d *= 2
	}
	if d > lockMax {
		d = lockMax
	}
	return now.Add(d)
}
