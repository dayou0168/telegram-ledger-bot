package bot

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func ledgerPermissionCacheKey(chatID, userID int64) string {
	return formatID(chatID) + ":" + formatID(userID)
}

func (b *Bot) globalOperatorCapabilities(ctx context.Context, userID int64) (permissions.UserCapabilities, bool, error) {
	memo := permissionMemoFromContext(ctx)
	if memo != nil {
		memo.mu.Lock()
		defer memo.mu.Unlock()
		if value, ok := memo.capabilities[userID]; ok {
			return value.Capabilities, value.Active, nil
		}
	}

	epoch, err := b.currentGlobalPermissionEpoch(ctx, memo)
	if err != nil {
		return permissions.UserCapabilities{}, false, err
	}
	if value, ok := b.globalCapabilityCache.Get(userID, epoch); ok {
		if memo != nil {
			memo.capabilities[userID] = value
		}
		return value.Capabilities, value.Active, nil
	}

	var value globalCapabilityValue
	if b.globalOperatorLookup != nil {
		value.Capabilities, value.Active, err = b.globalOperatorLookup(ctx, userID)
	} else {
		var level string
		level, value.Active, err = b.store.GetGlobalOperatorLevel(ctx, userID)
		if value.Active {
			value.Capabilities = permissions.UserCapabilities{GlobalOperatorLevel: level}
			value.Active = value.Capabilities.IsGlobalOperator()
		}
	}
	if err != nil {
		return permissions.UserCapabilities{}, false, err
	}
	b.globalCapabilityCache.Set(userID, epoch, value)
	if memo != nil {
		memo.capabilities[userID] = value
	}
	return value.Capabilities, value.Active, nil
}

func (b *Bot) globalOperatorCapabilitiesFresh(ctx context.Context, userID int64) (permissions.UserCapabilities, bool, error) {
	var (
		caps   permissions.UserCapabilities
		active bool
		err    error
	)
	if b.globalOperatorLookup != nil {
		caps, active, err = b.globalOperatorLookup(ctx, userID)
	} else if b.store != nil {
		var level string
		level, active, err = b.store.GetGlobalOperatorLevel(ctx, userID)
		if active {
			caps = permissions.UserCapabilities{GlobalOperatorLevel: level}
			active = caps.IsGlobalOperator()
		}
	}
	return caps, active, err
}

func (b *Bot) currentGlobalPermissionEpoch(ctx context.Context, memo *updatePermissionMemo) (int64, error) {
	if memo != nil && memo.epochLoaded {
		return memo.epoch, nil
	}
	var (
		epoch int64
		err   error
	)
	if b.permissionEpochLookup != nil {
		epoch, err = b.permissionEpochLookup(ctx)
	} else if b.store != nil {
		epoch, err = b.store.GetPermissionEpoch(ctx, storage.PermissionScopeGlobalOperator)
	} else {
		epoch = b.globalPermissionEpoch.Load()
		if epoch == 0 {
			epoch = 1
		}
	}
	if err != nil {
		return 0, err
	}
	b.applyGlobalPermissionEpoch(epoch)
	if memo != nil {
		memo.epochLoaded = true
		memo.epoch = epoch
	}
	return epoch, nil
}

func (b *Bot) applyGlobalPermissionEpoch(epoch int64) {
	if epoch <= 0 {
		return
	}
	for {
		current := b.globalPermissionEpoch.Load()
		if epoch <= current {
			return
		}
		if b.globalPermissionEpoch.CompareAndSwap(current, epoch) {
			b.globalCapabilityCache.Clear()
			return
		}
	}
}

func (b *Bot) permissionInvalidationScheduler(ctx context.Context) {
	if b.store == nil {
		return
	}
	for ctx.Err() == nil {
		err := b.store.ListenPermissionInvalidations(ctx, func(event storage.PermissionInvalidation) {
			if event.Scope == storage.PermissionScopeGlobalOperator {
				b.applyGlobalPermissionEpoch(event.Epoch)
			}
		})
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("permission invalidation listener: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (b *Bot) hasGlobalLedgerAccess(ctx context.Context, userID int64) (bool, error) {
	if b.perms.HasGlobalLedgerAccess(userID) {
		return true, nil
	}
	caps, ok, err := b.globalOperatorCapabilities(ctx, userID)
	if err != nil || !ok {
		return false, err
	}
	return b.perms.CanUseLedger(userID, caps), nil
}

func (b *Bot) InvalidateLedgerPermission(chatID, userID int64) {
	if b.operatorCache == nil {
		return
	}
	b.operatorCache.Delete(ledgerPermissionCacheKey(chatID, userID))
}

func (b *Bot) InvalidateBroadcastPermission(userID int64) {
	if b.privateStates != nil {
		b.privateStates.Delete(formatID(userID))
	}
	if b.store != nil && userID > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := b.clearBroadcastTarget(ctx, userID); err != nil {
			log.Printf("clear revoked broadcast target %d: %v", userID, err)
		}
	}
}

func (b *Bot) InvalidateAllPermissionCaches() {
	if b.globalCapabilityCache != nil {
		b.globalCapabilityCache.Clear()
	}
	if b.operatorCache != nil {
		b.operatorCache.Clear()
	}
	if b.privateStates != nil {
		b.privateStates.Clear()
	}
}

func (b *Bot) InvalidateWatchTargets() {
	if b.watchTargetCache != nil {
		b.watchTargetCache.Clear()
	}
}
