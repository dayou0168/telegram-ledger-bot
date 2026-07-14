package bot

import (
	"strings"
	"sync"
	"time"
)

type ttlCache[T any] struct {
	mu    sync.RWMutex
	items map[string]cacheItem[T]
	ttl   time.Duration
}

type cacheItem[T any] struct {
	value     T
	expiresAt time.Time
}

func newTTLCache[T any](ttl time.Duration) *ttlCache[T] {
	return &ttlCache[T]{
		items: make(map[string]cacheItem[T]),
		ttl:   ttl,
	}
}

func (c *ttlCache[T]) Get(key string) (T, bool) {
	var zero T
	if c.ttl <= 0 {
		return zero, false
	}
	now := time.Now()
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok || now.After(item.expiresAt) {
		if ok {
			c.mu.Lock()
			delete(c.items, key)
			c.mu.Unlock()
		}
		return zero, false
	}
	return item.value, true
}

func (c *ttlCache[T]) Set(key string, value T) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	c.items[key] = cacheItem[T]{value: value, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
}

func (c *ttlCache[T]) Delete(key string) {
	c.mu.Lock()
	delete(c.items, key)
	c.mu.Unlock()
}

func (c *ttlCache[T]) DeletePrefix(prefix string) {
	c.mu.Lock()
	for key := range c.items {
		if strings.HasPrefix(key, prefix) {
			delete(c.items, key)
		}
	}
	c.mu.Unlock()
}

func (c *ttlCache[T]) Clear() {
	c.mu.Lock()
	c.items = make(map[string]cacheItem[T])
	c.mu.Unlock()
}
