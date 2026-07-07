package bot

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func (b *Bot) handleBusinessMode(ctx context.Context, msg telegram.Message, user storage.User, open bool, now time.Time) error {
	if ok, err := b.canManageGroup(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "business_mode_denied", msg.Chat.ID, msg.MessageID, "没有上课/下课权限。", nil, now)
	}
	if err := b.store.SetGroupBusinessOpen(ctx, msg.Chat.ID, open, now); err != nil {
		return err
	}
	perms := groupSendPermissions(open)
	if err := b.tg.SetChatPermissions(ctx, msg.Chat.ID, perms); err != nil {
		_ = b.enqueueReplyText(ctx, sendPriorityNormal, "business_mode_failed", msg.Chat.ID, msg.MessageID, "设置群发言权限失败，请确认机器人是管理员并拥有管理群权限。", nil, now)
		return err
	}
	text := "已上课，全员发送消息权限已开启。"
	if !open {
		text = "已下课，全员发送消息权限已关闭。"
	}
	return b.enqueueReplyText(ctx, sendPriorityNormal, "business_mode_ok", msg.Chat.ID, msg.MessageID, text, nil, now)
}

func groupSendPermissions(open bool) telegram.ChatPermissions {
	return telegram.ChatPermissions{
		CanSendMessages:       open,
		CanSendAudios:         open,
		CanSendDocuments:      open,
		CanSendPhotos:         open,
		CanSendVideos:         open,
		CanSendVideoNotes:     open,
		CanSendVoiceNotes:     open,
		CanSendPolls:          open,
		CanSendOtherMessages:  open,
		CanAddWebPagePreviews: open,
	}
}

func (b *Bot) handleNotifyAll(ctx context.Context, msg telegram.Message, user storage.User) error {
	if ok, err := b.canManageGroup(ctx, msg.Chat.ID, user.ID); err != nil {
		return err
	} else if !ok {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "notify_all_denied", msg.Chat.ID, msg.MessageID, "没有通知所有人权限。", nil, time.Now().In(b.loc))
	}
	b.notifyAllInChatAsync(ctx, msg.Chat.ID, msg.MessageID)
	return nil
}

func (b *Bot) notifyAllInChatAsync(ctx context.Context, chatID, replyTo int64) {
	b.notifyPool.Submit(func(jobCtx context.Context) {
		users, err := b.store.ListUsersForMention(jobCtx, chatID, 500)
		if err != nil {
			log.Printf("list users for mention: %v", err)
			return
		}
		if len(users) == 0 {
			_ = b.enqueueReliableText(jobCtx, sendPriorityLow, "notify_all_empty", messageScopedDedupe("notify_all_empty", chatID, replyTo), chatID, "暂无可通知成员。", map[string]any{"reply_to_message_id": replyTo}, reliableMessageRef{}, time.Now().In(b.loc))
			return
		}
		for i, chunk := range mentionChunks(users, 3400) {
			if err := b.enqueueReliableText(jobCtx, sendPriorityLow, "notify_all_chunk", fmt.Sprintf("notify_all_chunk:%d:%d:%d", chatID, replyTo, i), chatID, chunk, map[string]any{
				"reply_to_message_id": replyTo,
				"parse_mode":          "HTML",
			}, reliableMessageRef{}, time.Now().In(b.loc)); err != nil {
				log.Printf("notify all enqueue: %v", err)
			}
		}
	})
}

func mentionChunks(users []storage.User, maxLen int) []string {
	var chunks []string
	var current strings.Builder
	current.WriteString("通知所有人：")
	for _, user := range users {
		item := " " + mentionUser(user)
		if current.Len()+len(item) > maxLen && current.Len() > len("通知所有人：") {
			chunks = append(chunks, current.String())
			current.Reset()
			current.WriteString("通知所有人：")
		}
		current.WriteString(item)
	}
	if current.Len() > len("通知所有人：") {
		chunks = append(chunks, current.String())
	}
	return chunks
}

func mentionUser(user storage.User) string {
	if user.Username != "" {
		return "@" + html.EscapeString(user.Username)
	}
	name := strings.TrimSpace(user.DisplayName)
	if name == "" {
		name = formatID(user.ID)
	}
	return `<a href="tg://user?id=` + formatID(user.ID) + `">` + html.EscapeString(name) + `</a>`
}

func isNotifyAllCommand(text string) bool {
	switch strings.TrimSpace(text) {
	case "通知所有人", "@所有人", "通知全员":
		return true
	default:
		return false
	}
}
