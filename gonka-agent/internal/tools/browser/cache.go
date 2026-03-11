package browser

import (
	"sync"
	"sync/atomic"
	"time"
)

type cacheEntry struct {
	content   string
	fetchedAt time.Time
}

// pageCache is a simple LRU cache for fetched page content.
// Entries expire after 5 minutes.
type pageCache struct {
	mu      sync.RWMutex
	items   map[string]cacheEntry
	maxSize int
	ttl     time.Duration
	hits    atomic.Int64
	misses  atomic.Int64
}

func newPageCache(maxSize int) *pageCache {
	return &pageCache{
		items:   make(map[string]cacheEntry, maxSize),
		maxSize: maxSize,
		ttl:     5 * time.Minute,
	}
}

func (c *pageCache) Get(url string) (string, bool) {
	c.mu.RLock()
	entry, ok := c.items[url]
	c.mu.RUnlock()

	if !ok {
		c.misses.Add(1)
		return "", false
	}
	if time.Since(entry.fetchedAt) > c.ttl {
		c.mu.Lock()
		delete(c.items, url)
		c.mu.Unlock()
		c.misses.Add(1)
		return "", false
	}
	c.hits.Add(1)
	return entry.content, true
}

func (c *pageCache) Set(url, content string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.items) >= c.maxSize {
		var oldest string
		var oldestTime time.Time
		for k, v := range c.items {
			if oldest == "" || v.fetchedAt.Before(oldestTime) {
				oldest = k
				oldestTime = v.fetchedAt
			}
		}
		delete(c.items, oldest)
	}

	c.items[url] = cacheEntry{content: content, fetchedAt: time.Now()}
}

func (c *pageCache) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}
