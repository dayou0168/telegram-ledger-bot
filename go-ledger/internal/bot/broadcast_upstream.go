package bot

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func (b *Bot) broadcastUpstreamRecipients(ctx context.Context, sourceUserID int64) ([]int64, error) {
	set := make(map[int64]struct{})
	hostID := b.perms.HostUserID()
	if sourceUserID != hostID {
		operator, ok, err := b.store.GetGlobalOperator(ctx, sourceUserID)
		if err != nil {
			return nil, err
		}
		if ok && operator.Status == "active" {
			switch operator.Level {
			case "primary":
				if hostID > 0 {
					set[hostID] = struct{}{}
				}
			case "secondary":
				resolved, resolveErr := b.store.ResolveOperatorMessageRecipients(ctx, sourceUserID, hostID)
				if resolveErr != nil {
					return nil, resolveErr
				}
				for _, recipientID := range resolved.Broadcast {
					if recipientID > 0 {
						set[recipientID] = struct{}{}
					}
				}
			}
		}
	}
	delete(set, sourceUserID)
	recipients := make([]int64, 0, len(set))
	for recipientID := range set {
		recipients = append(recipients, recipientID)
	}
	sort.Slice(recipients, func(i, j int) bool { return recipients[i] < recipients[j] })
	return recipients, nil
}

func (b *Bot) enqueueBroadcastUpstreamCopies(ctx context.Context, msg telegram.Message, sourceUserID int64, now time.Time) error {
	payload, ok := reliablePayloadFromMessage(msg)
	if !ok {
		return nil
	}
	recipients, err := b.broadcastUpstreamRecipients(ctx, sourceUserID)
	if err != nil {
		return err
	}
	for _, recipientID := range recipients {
		dedupeKey := fmt.Sprintf("broadcast_upstream:%d:%d:%d", msg.Chat.ID, msg.MessageID, recipientID)
		item, itemErr := reliablePayloadOutboxItem(sendPriorityNormal, "broadcast_upstream_copy", dedupeKey, recipientID, payload, nil, reliableMessageRef{})
		if itemErr != nil {
			return itemErr
		}
		inserted, _, enqueueErr := b.store.EnqueueBroadcastUpstreamMessage(ctx, item, sourceUserID, msg.Chat.ID, msg.MessageID, recipientID, now)
		if enqueueErr != nil {
			return enqueueErr
		}
		if inserted {
			b.kickNotificationOutbox()
		}
	}
	return nil
}
