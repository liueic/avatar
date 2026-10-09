package avatar

import (
	"container/list"
	"sync"
)

// LRU is a small generic least-recently-used byte cache. Keys are stringly
// typed composite cache keys; values are immutable byte slices shared by
// readers. It backs both the default-avatar cache and the server's scaled
// image cache.
type LRU struct {
	mu    sync.Mutex
	max   int
	ll    *list.List // of *lruEntry, front = most recent
	items map[string]*list.Element
}

type lruEntry struct {
	key   string
	value []byte
}

// NewLRU creates a cache holding at most max entries.
func NewLRU(max int) *LRU {
	if max < 1 {
		max = 1
	}
	return &LRU{max: max, ll: list.New(), items: make(map[string]*list.Element, max)}
}

// Get returns the cached value for key.
func (c *LRU) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		return el.Value.(*lruEntry).value, true
	}
	return nil, false
}

// Put stores value under key.
func (c *LRU) Put(key string, value []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*lruEntry).value = value
		return
	}
	c.items[key] = c.ll.PushFront(&lruEntry{key: key, value: value})
	if c.ll.Len() > c.max {
		if oldest := c.ll.Back(); oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(*lruEntry).key)
		}
	}
}

// Len reports the number of cached entries.
func (c *LRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
