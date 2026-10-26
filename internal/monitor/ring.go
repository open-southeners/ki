// Package monitor samples host and container resource usage and keeps a short
// in-memory history of the samples.
package monitor

import (
	"sync"
	"time"
)

// Ring is a fixed-resolution ring buffer of samples. Time is divided into
// slots of the configured resolution; adding a sample whose slot already
// holds one replaces it, so the latest sample in each slot wins. The buffer
// keeps at most history/resolution slots and is safe for concurrent use.
type Ring[T any] struct {
	mu    sync.Mutex
	res   time.Duration
	cap   int
	slots []ringSlot[T]
}

type ringSlot[T any] struct {
	at time.Time
	v  T
}

// NewRing returns a ring holding history/resolution slots (at least one).
// A non-positive resolution is treated as one second.
func NewRing[T any](history, resolution time.Duration) *Ring[T] {
	if resolution <= 0 {
		resolution = time.Second
	}
	n := int(history / resolution)
	if n < 1 {
		n = 1
	}
	return &Ring[T]{res: resolution, cap: n}
}

// Add stores v in the slot containing t. Samples older than the newest slot
// are ignored.
func (r *Ring[T]) Add(t time.Time, v T) {
	slot := t.Truncate(r.res)
	r.mu.Lock()
	defer r.mu.Unlock()
	if n := len(r.slots); n > 0 {
		last := r.slots[n-1].at
		if slot.Before(last) {
			return
		}
		if slot.Equal(last) {
			r.slots[n-1].v = v
			return
		}
	}
	if len(r.slots) >= r.cap {
		copy(r.slots, r.slots[1:])
		r.slots = r.slots[:len(r.slots)-1]
	}
	r.slots = append(r.slots, ringSlot[T]{at: slot, v: v})
}

// Snapshot returns the stored samples from oldest to newest.
func (r *Ring[T]) Snapshot() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]T, len(r.slots))
	for i, s := range r.slots {
		out[i] = s.v
	}
	return out
}

// Latest returns the newest stored sample.
func (r *Ring[T]) Latest() (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.slots) == 0 {
		var zero T
		return zero, false
	}
	return r.slots[len(r.slots)-1].v, true
}

// Len returns the number of stored samples.
func (r *Ring[T]) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.slots)
}
