package bot

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
)

func TestGlobalCapabilityCacheIsBoundedAndConcurrent(t *testing.T) {
	cache := newGlobalCapabilityCache(time.Minute, 2)
	value := globalCapabilityValue{Capabilities: permissions.UserCapabilities{GlobalOperatorLevel: "primary"}, Active: true}
	cache.Set(1, 1, value)
	cache.Set(2, 1, value)
	if _, ok := cache.Get(1, 1); !ok {
		t.Fatal("expected first entry before eviction")
	}
	cache.Set(3, 1, value)
	if _, ok := cache.Get(2, 1); ok {
		t.Fatal("least recently used entry was not evicted")
	}
	if cache.Len() != 2 {
		t.Fatalf("cache length = %d, want 2", cache.Len())
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(userID int64) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cache.Set(userID, 1, value)
				cache.Get(userID, 1)
			}
		}(int64(i + 10))
	}
	wg.Wait()
	if cache.Len() > 2 {
		t.Fatalf("cache length = %d, exceeds capacity", cache.Len())
	}
}

func TestGlobalCapabilityMemoAndEpochInvalidateRevocation(t *testing.T) {
	var epoch atomic.Int64
	epoch.Store(1)
	var epochReads atomic.Int32
	var capabilityReads atomic.Int32
	active := atomic.Bool{}
	active.Store(true)
	b := &Bot{
		globalCapabilityCache: newGlobalCapabilityCache(time.Hour, 16),
		permissionEpochLookup: func(context.Context) (int64, error) {
			epochReads.Add(1)
			return epoch.Load(), nil
		},
		globalOperatorLookup: func(context.Context, int64) (permissions.UserCapabilities, bool, error) {
			capabilityReads.Add(1)
			if !active.Load() {
				return permissions.UserCapabilities{}, false, nil
			}
			return permissions.UserCapabilities{GlobalOperatorLevel: "primary"}, true, nil
		},
	}

	firstUpdate := contextWithPermissionMemo(context.Background())
	for i := 0; i < 2; i++ {
		if _, ok, err := b.globalOperatorCapabilities(firstUpdate, 700000295280); err != nil || !ok {
			t.Fatalf("first update capability = %v, %v", ok, err)
		}
	}
	if epochReads.Load() != 1 || capabilityReads.Load() != 1 {
		t.Fatalf("same-update reads = epoch %d capability %d, want 1/1", epochReads.Load(), capabilityReads.Load())
	}

	secondUpdate := contextWithPermissionMemo(context.Background())
	if _, ok, err := b.globalOperatorCapabilities(secondUpdate, 700000295280); err != nil || !ok {
		t.Fatalf("second update cached capability = %v, %v", ok, err)
	}
	if epochReads.Load() != 2 || capabilityReads.Load() != 1 {
		t.Fatalf("cross-update reads = epoch %d capability %d, want 2/1", epochReads.Load(), capabilityReads.Load())
	}

	active.Store(false)
	epoch.Store(2)
	thirdUpdate := contextWithPermissionMemo(context.Background())
	if _, ok, err := b.globalOperatorCapabilities(thirdUpdate, 700000295280); err != nil || ok {
		t.Fatalf("revoked capability = %v, %v", ok, err)
	}
	if capabilityReads.Load() != 2 {
		t.Fatalf("capability reads after revocation = %d, want 2", capabilityReads.Load())
	}
}
