package mcp

import (
	"container/list"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// resultCache is a small bounded TTL cache for read-only tool results. The
// agent frequently repeats identical queries within one investigation; caching
// for a few seconds cuts redundant API-server load and latency without the
// unbounded memory of a full informer cache. Entries are evicted by TTL and by
// an LRU cap, so memory stays bounded regardless of query variety.
type resultCache struct {
	ttl   time.Duration
	max   int
	mu    sync.Mutex
	items map[string]*list.Element
	order *list.List // front = most recently used
	group singleflight.Group
}

type cacheEntry struct {
	key     string
	val     string
	expires time.Time
}

func newResultCache(ttl time.Duration, max int) *resultCache {
	return &resultCache{
		ttl:   ttl,
		max:   max,
		items: make(map[string]*list.Element, max),
		order: list.New(),
	}
}

func (c *resultCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*cacheEntry)
	if time.Now().After(e.expires) {
		c.removeElement(el)
		return "", false
	}
	c.order.MoveToFront(el)
	return e.val, true
}

func (c *resultCache) put(key, val string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		e := el.Value.(*cacheEntry)
		e.val, e.expires = val, time.Now().Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: key, val: val, expires: time.Now().Add(c.ttl)})
	c.items[key] = el
	for c.order.Len() > c.max {
		c.removeElement(c.order.Back())
	}
}

func (c *resultCache) removeElement(el *list.Element) {
	if el == nil {
		return
	}
	c.order.Remove(el)
	delete(c.items, el.Value.(*cacheEntry).key)
}

// do returns a cached result for key, or runs fn once (collapsing concurrent
// identical calls via singleflight) and caches the result. Errors are never
// cached.
func (c *resultCache) do(key string, fn func() (string, error)) (string, error) {
	if v, ok := c.get(key); ok {
		return v, nil
	}
	v, err, _ := c.group.Do(key, func() (any, error) {
		if v, ok := c.get(key); ok {
			return v, nil
		}
		out, err := fn()
		if err != nil {
			return "", err
		}
		c.put(key, out)
		return out, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}
