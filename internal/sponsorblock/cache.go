package sponsorblock

import (
	"sync"
	"time"
)

type cacheEntry[T any] struct {
	val T
	exp time.Time
}

type Cache[T any] struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]cacheEntry[T]
}

func NewCache[T any](ttl time.Duration) *Cache[T] {
	return &Cache[T]{ttl: ttl, m: make(map[string]cacheEntry[T])}
}

func (c *Cache[T]) Get(key string) (T, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero T
	ent, ok := c.m[key]
	if !ok || time.Now().After(ent.exp) {
		return zero, false
	}
	return ent.val, true
}

func (c *Cache[T]) Set(key string, val T) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key] = cacheEntry[T]{val: val, exp: time.Now().Add(c.ttl)}
}
