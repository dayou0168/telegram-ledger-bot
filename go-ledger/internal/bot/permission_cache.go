package bot

func ledgerPermissionCacheKey(chatID, userID int64) string {
	return formatID(chatID) + ":" + formatID(userID)
}

func broadcastPermissionCacheKey(userID int64) string {
	return "broadcast:" + formatID(userID)
}

func addressWatchPrivilegeCacheKey(userID int64) string {
	return "address_watch_unlimited:" + formatID(userID)
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
