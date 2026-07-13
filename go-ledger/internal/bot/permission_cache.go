package bot

import (
	"context"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
)

func ledgerPermissionCacheKey(chatID, userID int64) string {
	return formatID(chatID) + ":" + formatID(userID)
}

func broadcastPermissionCacheKey(userID int64) string {
	return "broadcast:" + formatID(userID)
}

func addressWatchPrivilegeCacheKey(userID int64) string {
	return "address_watch_unlimited:" + formatID(userID)
}

func (b *Bot) globalOperatorCapabilities(ctx context.Context, userID int64) (permissions.UserCapabilities, bool, error) {
	if b.globalOperatorLookup != nil {
		return b.globalOperatorLookup(ctx, userID)
	}
	level, ok, err := b.store.GetGlobalOperatorLevel(ctx, userID)
	if err != nil || !ok {
		return permissions.UserCapabilities{}, false, err
	}
	caps := permissions.UserCapabilities{GlobalOperatorLevel: level}
	return caps, caps.IsGlobalOperator(), nil
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
	b.operatorCache.Delete(addressWatchPrivilegeCacheKey(userID))
}

func (b *Bot) InvalidateBroadcastPermission(userID int64) {
	if b.operatorCache != nil {
		b.operatorCache.Delete(broadcastPermissionCacheKey(userID))
		b.operatorCache.Delete(addressWatchPrivilegeCacheKey(userID))
	}
	if b.privateStates != nil {
		b.privateStates.Delete(formatID(userID))
	}
}

func (b *Bot) InvalidateAllPermissionCaches() {
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
