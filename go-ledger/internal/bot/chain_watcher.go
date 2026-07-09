package bot

import (
	"context"
	"log"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainwatcher"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func (b *Bot) chainWatcherSyncScheduler(ctx context.Context) {
	b.syncAllChainWatcherSubscriptions(ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.syncAllChainWatcherSubscriptions(ctx)
		}
	}
}

func (b *Bot) chainWatcherEventScheduler(ctx context.Context) {
	interval := b.cfg.ChainWatcherPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		b.pollChainWatcherEvents(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (b *Bot) syncAllChainWatcherSubscriptions(ctx context.Context) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	targets, err := b.store.ListWatchTargets(ctx)
	if err != nil {
		log.Printf("list watch targets for chain watcher sync: %v", err)
		return
	}
	if err := b.watcher.SyncSubscriptions(ctx, targets); err != nil {
		log.Printf("sync chain watcher subscriptions: %v", err)
	}
}

func (b *Bot) syncChainWatcherTargetAsync(ctx context.Context, target storage.WatchTarget) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	b.dispatcher.Submit(ctx, "chain-watcher-sync", b.chainPool, func(jobCtx context.Context) {
		if err := b.watcher.UpsertSubscription(jobCtx, target); err != nil {
			log.Printf("sync chain watcher target %s: %v", target.Address, err)
		}
	})
}

func (b *Bot) deleteChainWatcherSubscriptionAsync(ctx context.Context, owner int64, address string) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	b.dispatcher.Submit(ctx, "chain-watcher-sync", b.chainPool, func(jobCtx context.Context) {
		if err := b.watcher.DeleteSubscription(jobCtx, owner, address); err != nil {
			log.Printf("delete chain watcher target %s: %v", address, err)
		}
	})
}

func (b *Bot) pollChainWatcherEvents(ctx context.Context) {
	if !b.cfg.ChainWatcherEnabled() {
		return
	}
	events, err := b.watcher.ClaimEvents(ctx, b.cfg.ChainWatcherBatchSize)
	if err != nil {
		log.Printf("claim chain watcher events: %v", err)
		return
	}
	if len(events) == 0 {
		return
	}
	acked := make([]string, 0, len(events))
	for _, event := range events {
		if err := b.processChainWatcherEvent(ctx, event); err != nil {
			log.Printf("process chain watcher event %s: %v", event.DeliveryID, err)
			continue
		}
		acked = append(acked, event.DeliveryID)
	}
	if len(acked) == 0 {
		return
	}
	if err := b.watcher.AckEvents(ctx, acked); err != nil {
		log.Printf("ack chain watcher events: %v", err)
	}
}

func (b *Bot) processChainWatcherEvent(ctx context.Context, event chainwatcher.MatchedEvent) error {
	transfer := tron.Transfer{
		Hash:           event.TxHash,
		From:           event.From,
		To:             event.To,
		Value:          event.Value,
		TokenSymbol:    event.TokenSymbol,
		TokenAddress:   event.TokenAddress,
		TokenDecimals:  event.TokenDecimals,
		BlockTimestamp: event.BlockTimestamp,
		Confirmed:      event.Confirmed,
	}
	target := storage.WatchTarget{
		OwnerUserID:     event.OwnerUserID,
		Address:         event.WatchAddress,
		Label:           event.Label,
		WatchIncome:     event.Direction == "income",
		WatchExpense:    event.Direction == "expense",
		MinNotifyAmount: "0",
	}
	text := b.formatTransferNotice(transfer, target, event.Direction)
	chatID := event.ChatID
	if chatID == 0 {
		chatID = event.OwnerUserID
	}
	inserted, err := b.store.RecordChainNotificationOutbox(ctx, event.OwnerUserID, event.WatchAddress, event.TxHash, event.Direction, event.BlockTimestamp, chatID, text, "HTML", true, time.Now().In(b.loc))
	if err != nil {
		return err
	}
	if inserted {
		b.kickNotificationOutbox()
	}
	return nil
}
