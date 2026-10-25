package webhooks

import (
	"container/list"
	"sync"
	"time"
)

// dedupe is a bounded LRU of delivery ids with a time-to-live.
type dedupe struct {
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	now   func() time.Time
	order *list.List // front = most recent; values are *dedupeEntry
	items map[string]*list.Element
}

type dedupeEntry struct {
	key string
	at  time.Time
}

func newDedupe(max int, ttl time.Duration) *dedupe {
	return &dedupe{max: max, ttl: ttl, now: time.Now, order: list.New(), items: map[string]*list.Element{}}
}

// has reports whether key was added within the TTL.
func (d *dedupe) has(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.items[key]
	if !ok {
		return false
	}
	if d.now().Sub(el.Value.(*dedupeEntry).at) > d.ttl {
		d.order.Remove(el)
		delete(d.items, key)
		return false
	}
	d.order.MoveToFront(el)
	return true
}

// add records key, evicting the least recently used entry beyond the cap.
func (d *dedupe) add(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if el, ok := d.items[key]; ok {
		el.Value.(*dedupeEntry).at = d.now()
		d.order.MoveToFront(el)
		return
	}
	d.items[key] = d.order.PushFront(&dedupeEntry{key: key, at: d.now()})
	for d.order.Len() > d.max {
		last := d.order.Back()
		d.order.Remove(last)
		delete(d.items, last.Value.(*dedupeEntry).key)
	}
}
