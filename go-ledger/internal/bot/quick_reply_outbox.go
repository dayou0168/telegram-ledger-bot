package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

const (
	quickReplyOutboxBatchSize  = 20
	quickReplyOutboxMaxAttempt = 8
	quickReplyOutboxLease      = 2 * time.Minute
	quickReplyOutboxRetention  = 72 * time.Hour
)

var (
	errQuickReplyPermissionRevoked = errors.New("quick reply permission revoked")
	errQuickReplyLeaseLost         = errors.New("quick reply outbox lease lost")
)

type quickReplyOutboxLeaseGuard struct {
	bot      *Bot
	itemID   int64
	owner    string
	lease    time.Duration
	stop     chan struct{}
	lost     chan struct{}
	stopOnce sync.Once
	lostOnce sync.Once
	lostFlag atomic.Bool
	mu       sync.Mutex
	deadline time.Time
}

func (b *Bot) kickQuickReplyOutbox() {
	if b == nil || b.quickReplyWake == nil {
		return
	}
	select {
	case b.quickReplyWake <- struct{}{}:
	default:
	}
}

func (b *Bot) quickReplyOutboxScheduler(ctx context.Context) {
	owner := fmt.Sprintf("%s:%d", b.telegramInboxStreamKey(), time.Now().UnixNano())
	ticker := time.NewTicker(500 * time.Millisecond)
	cleanupTicker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	defer cleanupTicker.Stop()
	b.drainQuickReplyOutbox(ctx, owner)
	b.cleanupQuickReplyOutbox(ctx, time.Now().In(b.loc))
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.quickReplyWake:
			b.drainQuickReplyOutbox(ctx, owner)
		case <-ticker.C:
			b.drainQuickReplyOutbox(ctx, owner)
		case now := <-cleanupTicker.C:
			b.cleanupQuickReplyOutbox(ctx, now.In(b.loc))
		}
	}
}

func (b *Bot) drainQuickReplyOutbox(ctx context.Context, owner string) {
	for round := 0; round < 3 && ctx.Err() == nil; round++ {
		lease := b.quickReplyLease
		if lease <= 0 {
			lease = quickReplyOutboxLease
		}
		items, err := b.store.ClaimQuickReplyOutbox(ctx, owner, quickReplyOutboxBatchSize,
			quickReplyOutboxMaxAttempt, lease, time.Now().In(b.loc))
		if err != nil {
			log.Printf("claim quick reply outbox: %v", err)
			return
		}
		if len(items) == 0 {
			return
		}
		for _, item := range items {
			item := item
			guard := b.startQuickReplyOutboxLeaseGuard(ctx, item, owner)
			if !b.quickReplyPool.Submit(func(sendCtx context.Context) {
				defer guard.Stop()
				b.sendQuickReplyOutbox(sendCtx, item, owner, guard)
			}) {
				now := time.Now().In(b.loc)
				_, retryErr := b.store.RetryQuickReplyOutbox(ctx, item.ID, owner, quickReplyOutboxMaxAttempt,
					now.Add(time.Second), now, errors.New("quick reply worker queue is full"))
				if retryErr != nil {
					log.Printf("release full quick reply outbox %d: %v", item.ID, retryErr)
				}
				guard.Stop()
			}
		}
		if len(items) < quickReplyOutboxBatchSize {
			return
		}
	}
}

func (b *Bot) sendQuickReplyOutbox(ctx context.Context, item storage.QuickReplyOutbox, owner string, guard *quickReplyOutboxLeaseGuard) {
	if guard.Lost() {
		return
	}
	if b.sendGateway == nil || b.tg == nil {
		b.retryQuickReplyOutbox(ctx, item, owner, guard, errTelegramSendGatewayNotConfigured)
		return
	}
	opts := map[string]any{"reply_to_message_id": item.TargetMessageID}
	_, err := b.sendGateway.DoOnce(ctx, sendPriorityNormal, item.TargetChatID, func(opCtx context.Context) (telegram.Message, error) {
		if guard.Lost() {
			return telegram.Message{}, errQuickReplyLeaseLost
		}
		allowed, checkErr := b.quickReplyOutboxPermissionFresh(opCtx, item)
		if checkErr != nil {
			return telegram.Message{}, fmt.Errorf("check quick reply permission: %w", checkErr)
		}
		if !allowed {
			return telegram.Message{}, errQuickReplyPermissionRevoked
		}
		if guard.Lost() {
			return telegram.Message{}, errQuickReplyLeaseLost
		}
		return b.tg.CopyMessage(opCtx, item.TargetChatID, item.SourceChatID, item.SourceMessageID, cloneSendOptions(opts))
	})
	if errors.Is(err, errQuickReplyLeaseLost) {
		return
	}
	if errors.Is(err, errQuickReplyPermissionRevoked) {
		now := time.Now().In(b.loc)
		cancelled, _, cancelErr := b.store.CancelQuickReplyOutboxRevoked(ctx, item.ID, owner,
			"quick reply permission revoked before delivery", now)
		if cancelErr != nil {
			b.retryQuickReplyOutbox(ctx, item, owner, guard, cancelErr)
			return
		}
		if cancelled {
			if enqueueErr := b.enqueueReliableText(ctx, sendPriorityNormal, "quick_reply_lost",
				fmt.Sprintf("quick_reply_lost:%d", item.ID), item.SourceChatID,
				"快速回复目标已失效，请重新点回复通知。",
				map[string]any{"reply_to_message_id": item.SourceMessageID}, reliableMessageRef{}, now); enqueueErr != nil {
				log.Printf("enqueue revoked quick reply notice %d: %v", item.ID, enqueueErr)
			}
		}
		return
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		if !telegramGatewayRetryable(err) {
			b.deadQuickReplyOutbox(ctx, item, owner, guard, err)
			return
		}
		b.retryQuickReplyOutbox(ctx, item, owner, guard, err)
		return
	}
	now := time.Now().In(b.loc)
	ok, markErr := b.store.MarkQuickReplyOutboxSent(ctx, item.ID, owner, now)
	if markErr != nil {
		log.Printf("mark quick reply outbox %d sent: %v", item.ID, markErr)
	} else if !ok {
		guard.MarkLost()
	}
}

func (b *Bot) quickReplyOutboxPermissionFresh(ctx context.Context, item storage.QuickReplyOutbox) (bool, error) {
	state := privateState{QuickReplyTargetChat: item.TargetChatID, QuickReplyMessageID: item.TargetMessageID}
	delivery, allowed, err := b.quickReplyDeliveryFresh(ctx, item.ActorUserID, state)
	if err != nil || !allowed {
		return allowed, err
	}
	return delivery.TargetChatID == item.TargetChatID && delivery.TargetMessageID == item.TargetMessageID, nil
}

func (b *Bot) retryQuickReplyOutbox(ctx context.Context, item storage.QuickReplyOutbox, owner string, guard *quickReplyOutboxLeaseGuard, cause error) {
	if guard.Lost() {
		return
	}
	now := time.Now().In(b.loc)
	delay := notificationRetryDelay(item.Attempts, cause)
	status, err := b.store.RetryQuickReplyOutbox(ctx, item.ID, owner, quickReplyOutboxMaxAttempt, now.Add(delay), now, cause)
	if err != nil {
		log.Printf("retry quick reply outbox %d: %v", item.ID, err)
		return
	}
	if status == "" {
		guard.MarkLost()
		return
	}
	if status == "dead" {
		b.enqueueQuickReplyDeadNotice(ctx, item, cause, now)
	}
}

func (b *Bot) deadQuickReplyOutbox(ctx context.Context, item storage.QuickReplyOutbox, owner string, guard *quickReplyOutboxLeaseGuard, cause error) {
	if guard.Lost() {
		return
	}
	now := time.Now().In(b.loc)
	ok, err := b.store.MarkQuickReplyOutboxDead(ctx, item.ID, owner, now, cause)
	if err != nil {
		log.Printf("mark quick reply outbox %d dead: %v", item.ID, err)
		return
	}
	if !ok {
		guard.MarkLost()
		return
	}
	b.enqueueQuickReplyDeadNotice(ctx, item, cause, now)
}

func (b *Bot) enqueueQuickReplyDeadNotice(ctx context.Context, item storage.QuickReplyOutbox, cause error, now time.Time) {
	if enqueueErr := b.enqueueReliableText(ctx, sendPriorityNormal, "quick_reply_failed",
		fmt.Sprintf("quick_reply_dead:%d", item.ID), item.SourceChatID,
		"快速回复发送失败："+cause.Error(),
		map[string]any{"reply_to_message_id": item.SourceMessageID}, reliableMessageRef{}, now); enqueueErr != nil {
		log.Printf("enqueue dead quick reply notice %d: %v", item.ID, enqueueErr)
	}
}

func (b *Bot) cleanupQuickReplyOutbox(ctx context.Context, now time.Time) {
	retention := b.cfg.OutboxSentRetention
	if retention <= 0 {
		retention = quickReplyOutboxRetention
	}
	for {
		removed, err := b.store.CleanupQuickReplyOutbox(ctx, now.Add(-retention), 2000)
		if err != nil {
			log.Printf("cleanup quick reply outbox: %v", err)
			return
		}
		if removed < 2000 {
			return
		}
	}
}

func (b *Bot) startQuickReplyOutboxLeaseGuard(ctx context.Context, item storage.QuickReplyOutbox, owner string) *quickReplyOutboxLeaseGuard {
	lease := b.quickReplyLease
	if lease <= 0 {
		lease = quickReplyOutboxLease
	}
	deadline := time.Now().Add(lease)
	if item.LeaseUntil != nil {
		deadline = *item.LeaseUntil
	}
	guard := &quickReplyOutboxLeaseGuard{
		bot: b, itemID: item.ID, owner: owner, lease: lease,
		stop: make(chan struct{}), lost: make(chan struct{}), deadline: deadline,
	}
	go guard.run(ctx)
	return guard
}

func (g *quickReplyOutboxLeaseGuard) run(ctx context.Context) {
	ticker := time.NewTicker(g.lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-g.stop:
			return
		case <-ticker.C:
		}
		now := time.Now().In(g.bot.loc)
		if !g.Deadline().After(now) {
			g.MarkLost()
			return
		}
		renewCtx, cancel := context.WithTimeout(ctx, g.lease/2)
		ok, err := g.bot.store.RenewQuickReplyOutboxLease(renewCtx, g.itemID, g.owner, g.lease, now)
		cancel()
		if err != nil {
			log.Printf("renew quick reply outbox %d: %v", g.itemID, err)
			if !g.Deadline().After(time.Now()) {
				g.MarkLost()
				return
			}
			continue
		}
		if !ok {
			g.MarkLost()
			return
		}
		g.SetDeadline(now.Add(g.lease))
	}
}

func (g *quickReplyOutboxLeaseGuard) Stop() {
	g.stopOnce.Do(func() { close(g.stop) })
}

func (g *quickReplyOutboxLeaseGuard) MarkLost() {
	g.lostFlag.Store(true)
	g.lostOnce.Do(func() { close(g.lost) })
}

func (g *quickReplyOutboxLeaseGuard) Lost() bool {
	return g == nil || g.lostFlag.Load()
}

func (g *quickReplyOutboxLeaseGuard) Deadline() time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.deadline
}

func (g *quickReplyOutboxLeaseGuard) SetDeadline(value time.Time) {
	g.mu.Lock()
	g.deadline = value
	g.mu.Unlock()
}
