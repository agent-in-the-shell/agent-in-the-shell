package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// memory is a bounded, TTL-aware LRU cache. The doubly-linked list orders
// entries by recency (front = most recent); the map gives O(1) lookup. Expiry
// is lazy (checked on read) plus opportunistic (an expired entry found on read
// is dropped), which is sufficient for a read-through response cache where every
// useful entry is read before it matters.
type memory struct {
	mu    sync.Mutex
	cap   int
	ttl   time.Duration
	clock func() time.Time
	ll    *list.List // *memEntry, front = most-recently-used
	items map[string]*list.Element
}

type memEntry struct {
	key     string
	value   []byte
	expires time.Time // zero = never expires
}

func newMemory(capacity int, ttl time.Duration, clock func() time.Time) *memory {
	return &memory{
		cap:   capacity,
		ttl:   ttl,
		clock: clock,
		ll:    list.New(),
		items: make(map[string]*list.Element, capacity),
	}
}

func (m *memory) expired(e *memEntry) bool {
	return !e.expires.IsZero() && m.clock().After(e.expires)
}

func (m *memory) Get(_ context.Context, key string) ([]byte, bool) {
	m.mu.Lock()
	el, ok := m.items[key]
	if !ok {
		m.mu.Unlock()
		return nil, false
	}
	e := el.Value.(*memEntry)
	if m.expired(e) {
		m.removeElement(el)
		m.mu.Unlock()
		return nil, false
	}
	m.ll.MoveToFront(el)
	val := e.value // Set publishes a fresh slice and never mutates one in place,
	m.mu.Unlock()  // so this pointer stays immutable once we release the lock.
	return cloneBytes(val), true
}

func (m *memory) Set(_ context.Context, key string, value []byte) {
	stored := cloneBytes(value) // copy off the critical section
	m.mu.Lock()
	defer m.mu.Unlock()
	var exp time.Time
	if m.ttl > 0 {
		exp = m.clock().Add(m.ttl)
	}
	if el, ok := m.items[key]; ok {
		e := el.Value.(*memEntry)
		e.value = stored
		e.expires = exp
		m.ll.MoveToFront(el)
		return
	}
	el := m.ll.PushFront(&memEntry{key: key, value: stored, expires: exp})
	m.items[key] = el
	for m.ll.Len() > m.cap {
		m.removeOldest()
	}
}

func (m *memory) removeOldest() {
	if el := m.ll.Back(); el != nil {
		m.removeElement(el)
	}
}

func (m *memory) removeElement(el *list.Element) {
	m.ll.Remove(el)
	delete(m.items, el.Value.(*memEntry).key)
}

func (m *memory) Close() error { return nil }
