package bot

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
)

type globalCapabilityValue struct {
	Capabilities permissions.UserCapabilities
	Active       bool
}

type globalCapabilityCacheEntry struct {
	userID    int64
	value     globalCapabilityValue
	epoch     int64
	expiresAt time.Time
}

type globalCapabilityCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	capacity int
	items    map[int64]*list.Element
	lru      *list.List
}

func newGlobalCapabilityCache(ttl time.Duration, capacity int) *globalCapabilityCache {
	if capacity < 1 {
		capacity = 1
	}
	return &globalCapabilityCache{
		ttl:      ttl,
		capacity: capacity,
		items:    make(map[int64]*list.Element),
		lru:      list.New(),
	}
}

func (c *globalCapabilityCache) Get(userID, epoch int64) (globalCapabilityValue, bool) {
	var zero globalCapabilityValue
	if c == nil || c.ttl <= 0 {
		return zero, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[userID]
	if !ok {
		return zero, false
	}
	entry := element.Value.(globalCapabilityCacheEntry)
	if entry.epoch != epoch || !time.Now().Before(entry.expiresAt) {
		delete(c.items, userID)
		c.lru.Remove(element)
		return zero, false
	}
	c.lru.MoveToFront(element)
	return entry.value, true
}

func (c *globalCapabilityCache) Set(userID, epoch int64, value globalCapabilityValue) {
	if c == nil || c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := globalCapabilityCacheEntry{userID: userID, value: value, epoch: epoch, expiresAt: time.Now().Add(c.ttl)}
	if element, ok := c.items[userID]; ok {
		element.Value = entry
		c.lru.MoveToFront(element)
		return
	}
	element := c.lru.PushFront(entry)
	c.items[userID] = element
	for c.lru.Len() > c.capacity {
		oldest := c.lru.Back()
		oldEntry := oldest.Value.(globalCapabilityCacheEntry)
		delete(c.items, oldEntry.userID)
		c.lru.Remove(oldest)
	}
}

func (c *globalCapabilityCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = make(map[int64]*list.Element)
	c.lru.Init()
	c.mu.Unlock()
}

func (c *globalCapabilityCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}

type permissionMemoContextKey struct{}

type updatePermissionMemo struct {
	mu           sync.Mutex
	epochLoaded  bool
	epoch        int64
	capabilities map[int64]globalCapabilityValue
}

func contextWithPermissionMemo(ctx context.Context) context.Context {
	return context.WithValue(ctx, permissionMemoContextKey{}, &updatePermissionMemo{
		capabilities: make(map[int64]globalCapabilityValue),
	})
}

func permissionMemoFromContext(ctx context.Context) *updatePermissionMemo {
	memo, _ := ctx.Value(permissionMemoContextKey{}).(*updatePermissionMemo)
	return memo
}
