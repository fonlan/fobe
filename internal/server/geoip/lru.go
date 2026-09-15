package geoip

import (
	"container/list"
	"sync"
)

// lru is a small mutex-guarded LRU of ip → country code used by Online.
type lru struct {
	cap int
	mu  sync.Mutex
	ll  *list.List               // front = most recently used
	els map[string]*list.Element // key -> element holding lruItem
}

type lruItem struct {
	key  string
	code string
}

func newLRU(capacity int) *lru {
	if capacity < 1 {
		capacity = 1
	}
	return &lru{cap: capacity, ll: list.New(), els: make(map[string]*list.Element, capacity)}
}

func (c *lru) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.els[key]
	if !ok {
		return "", false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*lruItem).code, true
}

func (c *lru) put(key, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.els[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*lruItem).code = code
		return
	}
	c.els[key] = c.ll.PushFront(&lruItem{key: key, code: code})
	for len(c.els) > c.cap {
		oldest := c.ll.Back()
		if oldest == nil {
			return
		}
		c.ll.Remove(oldest)
		delete(c.els, oldest.Value.(*lruItem).key)
	}
}
