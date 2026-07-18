package bot

import (
	"context"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func isBroadcastTargetState(state privateState) bool {
	switch state.Mode {
	case "all":
		return true
	case "group":
		return state.GroupID > 0 || (state.TargetName != "" && len(state.ChatIDs) > 0)
	case "chat":
		return state.TargetChatID != 0 || len(state.ChatIDs) == 1
	default:
		return false
	}
}

func hasPersistentBroadcastTargetFields(state privateState) bool {
	switch state.Mode {
	case "all":
		return true
	case "group":
		return state.GroupID > 0
	case "chat":
		return state.TargetChatID != 0
	default:
		return false
	}
}

func (b *Bot) saveBroadcastTarget(ctx context.Context, userID int64, state privateState) error {
	if !hasPersistentBroadcastTargetFields(state) {
		return b.clearBroadcastTarget(ctx, userID)
	}
	return b.store.UpsertTelegramBroadcastTarget(ctx, storage.TelegramBroadcastTarget{
		StreamKey: b.telegramInboxStreamKey(), UserID: userID, Mode: state.Mode,
		ChatID: state.TargetChatID, GroupID: state.GroupID, TargetName: state.TargetName,
		NotifyAll: state.NotifyAll,
	}, time.Now().In(b.loc))
}

func (b *Bot) clearBroadcastTarget(ctx context.Context, userID int64) error {
	return b.store.DeleteTelegramBroadcastTarget(ctx, b.telegramInboxStreamKey(), userID)
}

func privateStateFromBroadcastTarget(target storage.TelegramBroadcastTarget) privateState {
	return privateState{
		Mode: target.Mode, TargetName: target.TargetName, TargetChatID: target.ChatID,
		GroupID: target.GroupID, NotifyAll: target.NotifyAll, CreatedAt: target.UpdatedAt,
	}
}

func (b *Bot) loadBroadcastTargetState(ctx context.Context, userID int64) (privateState, bool, error) {
	target, ok, err := b.store.GetTelegramBroadcastTarget(ctx, b.telegramInboxStreamKey(), userID)
	if err != nil || !ok {
		return privateState{}, false, err
	}
	state := privateStateFromBroadcastTarget(target)
	if !isBroadcastTargetState(state) {
		return privateState{}, false, nil
	}
	return state, true, nil
}

func (b *Bot) upgradeLegacyBroadcastState(ctx context.Context, userID int64, state privateState) (privateState, error) {
	if state.Mode == "quick_reply" || state.Mode == "" {
		return state, nil
	}
	switch state.Mode {
	case "chat":
		if len(state.ChatIDs) == 1 {
			state.TargetChatID = state.ChatIDs[0]
		}
	case "group":
		group, ok, err := b.store.GetBroadcastGroup(ctx, state.TargetName)
		if err != nil {
			return state, err
		}
		if ok {
			state.GroupID = group.ID
		}
	}
	if hasPersistentBroadcastTargetFields(state) {
		if err := b.saveBroadcastTarget(ctx, userID, state); err != nil {
			return state, err
		}
	}
	return state, nil
}
