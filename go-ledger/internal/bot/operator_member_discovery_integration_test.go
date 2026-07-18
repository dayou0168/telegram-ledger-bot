package bot

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestPostgresTelegramIngressPersistsIdentityBeforeInboxHandling(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := time.Now().UnixNano()
	chatID := -base
	userID := base%1_000_000_000 + 720_000_000_000
	updateID := base%1_000_000_000 + 1000
	eventTime := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	b := New(telegramInboxIntegrationConfig(fmt.Sprintf("member-ingress-%d", base)), store, nil, nil, nil)

	memberUpdate := telegram.Update{UpdateID: updateID, ChatMember: &telegram.ChatMemberUpd{
		Chat: telegram.Chat{ID: chatID, Type: "supergroup", Title: "member ingress"},
		Date: eventTime.Unix(),
		NewChatMember: telegram.ChatMember{Status: "member", User: telegram.User{
			ID: userID, Username: "member_ingress", FirstName: "Member",
		}},
	}}
	if _, err := b.persistTelegramUpdateBatch(ctx, []telegram.Update{memberUpdate}); err != nil {
		t.Fatalf("persist chat_member update: %v", err)
	}
	found, ok, err := store.FindUserByUsername(ctx, chatID, "member_ingress")
	if err != nil || !ok || found.ID != userID {
		t.Fatalf("chat_member identity=%+v ok=%v err=%v", found, ok, err)
	}
	mentioned, err := store.ListUsersForMention(ctx, chatID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mentioned) != 0 {
		t.Fatalf("chat_member identity entered spoken audience: %+v", mentioned)
	}

	messageUpdate := telegram.Update{UpdateID: updateID + 1, Message: &telegram.Message{
		MessageID: 2,
		Date:      eventTime.Add(time.Second).Unix(),
		Chat:      telegram.Chat{ID: chatID, Type: "supergroup", Title: "member ingress"},
		From:      &telegram.User{ID: userID, Username: "member_ingress", FirstName: "Member"},
		Text:      "+1",
	}}
	if _, err := b.persistTelegramUpdateBatch(ctx, []telegram.Update{messageUpdate}); err != nil {
		t.Fatalf("persist message update: %v", err)
	}
	mentioned, err = store.ListUsersForMention(ctx, chatID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(mentioned) != 1 || mentioned[0].ID != userID {
		t.Fatalf("message sender missing from spoken audience before inbox handling: %+v", mentioned)
	}
}

func TestPostgresOperatorSetupReplyUsernameUnknownAndLocalBoundary(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := time.Now().UnixNano()
	chatID := -base
	ownerID := base%1_000_000_000 + 730_000_000_000
	replyTargetID := ownerID + 1
	knownTargetID := ownerID + 2
	textMentionTargetID := ownerID + 3
	strangerID := ownerID + 4
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := storage.User{ID: ownerID, Username: "owner", DisplayName: "Owner"}
	if err := store.EnsureGroup(ctx, chatID, "operator setup", now); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchUser(ctx, chatID, owner, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupOwner(ctx, chatID, owner, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveUserForTelegramUpdate(ctx, chatID, storage.User{
		ID: knownTargetID, Username: "known_target", DisplayName: "Known Target",
	}, 10, now); err != nil {
		t.Fatal(err)
	}

	b := New(telegramInboxIntegrationConfig(fmt.Sprintf("operator-setup-%d", base)), store, nil, nil, nil)
	b.globalOperatorLookup = func(context.Context, int64) (permissions.UserCapabilities, bool, error) {
		return permissions.UserCapabilities{}, false, nil
	}
	message := func(updateID, messageID, fromID int64, username, text string) (context.Context, telegram.Message) {
		msgCtx := context.WithValue(ctx, telegramUpdateIDContextKey{}, updateID)
		return msgCtx, telegram.Message{
			MessageID: messageID,
			Date:      now.Unix(),
			Chat:      telegram.Chat{ID: chatID, Type: "supergroup", Title: "operator setup"},
			From:      &telegram.User{ID: fromID, Username: username, FirstName: username},
			Text:      text,
		}
	}
	claim := func(messageID int64) storage.NotificationOutbox {
		t.Helper()
		items, claimErr := store.ClaimDueNotifications(ctx, 100, 5, time.Now().Add(time.Minute))
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		for _, item := range items {
			if item.ChatID == chatID && strings.Contains(item.DedupeKey, ":"+formatID(messageID)) {
				if err := store.MarkNotificationSent(ctx, item.ID, messageID+10000, time.Now()); err != nil {
					t.Fatal(err)
				}
				return item
			}
		}
		t.Fatalf("notification for message %d not found: %+v", messageID, items)
		return storage.NotificationOutbox{}
	}

	replyCtx, replyCommand := message(101, 1001, ownerID, "owner", "设置操作人")
	replyCommand.ReplyTo = &telegram.Message{
		MessageID: 900,
		Date:      now.Add(-time.Minute).Unix(),
		Chat:      replyCommand.Chat,
		From:      &telegram.User{ID: replyTargetID, FirstName: "Reply Target"},
		Text:      "hello",
	}
	if err := b.handleMessage(replyCtx, replyCommand); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.IsOperator(ctx, chatID, replyTargetID); err != nil || !ok {
		t.Fatalf("reply target operator=%v err=%v", ok, err)
	}
	claim(replyCommand.MessageID)

	mentionCtx, mentionCommand := message(102, 1002, ownerID, "owner", "设置操作人 无用户名成员")
	mentionCommand.Entities = []telegram.MessageEntity{{
		Type: "text_mention", Offset: 999, Length: 0,
		User: &telegram.User{ID: textMentionTargetID, FirstName: "No Username"},
	}}
	if err := b.handleMessage(mentionCtx, mentionCommand); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.IsOperator(ctx, chatID, textMentionTargetID); err != nil || !ok {
		t.Fatalf("text_mention target operator=%v err=%v", ok, err)
	}
	claim(mentionCommand.MessageID)

	knownCtx, knownCommand := message(103, 1003, ownerID, "owner", "设置操作人 @known_target")
	if err := b.handleMessage(knownCtx, knownCommand); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.IsOperator(ctx, chatID, knownTargetID); err != nil || !ok {
		t.Fatalf("known username operator=%v err=%v", ok, err)
	}
	claim(knownCommand.MessageID)

	duplicateCommand := knownCommand
	duplicateCommand.ReplyTo = &telegram.Message{From: &telegram.User{
		ID: knownTargetID, Username: "known_target", FirstName: "Known Target",
	}}
	duplicateCommand.Entities = []telegram.MessageEntity{
		{Type: "text_mention", Offset: -10, Length: 500, User: duplicateCommand.ReplyTo.From},
		{Type: "text_mention", User: &telegram.User{ID: ownerID, FirstName: "Owner"}},
		{Type: "text_mention", User: &telegram.User{ID: 0, FirstName: "Invalid"}},
		{Type: "text_mention", User: &telegram.User{ID: ownerID + 99, FirstName: "Bot", IsBot: true}},
	}
	targets, missing, err := b.operatorTargets(ctx, duplicateCommand, duplicateCommand.Text)
	if err != nil || len(missing) != 0 || len(targets) != 1 || targets[0].ID != knownTargetID {
		t.Fatalf("deduplicated targets=%+v missing=%v err=%v", targets, missing, err)
	}

	unknownCtx, unknownCommand := message(104, 1004, ownerID, "owner", "设置操作人 @never_seen")
	if err := b.handleMessage(unknownCtx, unknownCommand); err != nil {
		t.Fatal(err)
	}
	unknownNotice := claim(unknownCommand.MessageID)
	if !strings.Contains(unknownNotice.Text, "机器人尚未获取到该用户信息") ||
		!strings.Contains(unknownNotice.Text, "回复对方的群消息") ||
		!strings.Contains(unknownNotice.Text, "先在群里发言") ||
		!strings.Contains(unknownNotice.Text, "@用户名") {
		t.Fatalf("unknown username guidance is unclear: %q", unknownNotice.Text)
	}

	plainCtx, plainCommand := message(105, 1005, ownerID, "owner", "设置操作人 普通昵称")
	if err := b.handleMessage(plainCtx, plainCommand); err != nil {
		t.Fatal(err)
	}
	plainNotice := claim(plainCommand.MessageID)
	if !strings.Contains(plainNotice.Text, "请回复对方消息，或输入 @用户名") {
		t.Fatalf("plain nickname was not rejected clearly: %q", plainNotice.Text)
	}

	localCtx, localCommand := message(106, 1006, replyTargetID, "reply_target", "设置操作人")
	localCommand.ReplyTo = &telegram.Message{
		MessageID: 901,
		Date:      now.Unix(),
		Chat:      localCommand.Chat,
		From:      &telegram.User{ID: strangerID, Username: "stranger", FirstName: "Stranger"},
		Text:      "hello",
	}
	if err := b.handleMessage(localCtx, localCommand); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.IsOperator(ctx, chatID, strangerID); err != nil || ok {
		t.Fatalf("single-group operator delegated permission=%v err=%v", ok, err)
	}
	deniedNotice := claim(localCommand.MessageID)
	if !strings.Contains(deniedNotice.Text, "只有宿主或本群最高权限") {
		t.Fatalf("unexpected local delegation denial: %q", deniedNotice.Text)
	}
	if ok, err := b.canUseLedgerFresh(ctx, chatID, replyTargetID); err != nil || !ok {
		t.Fatalf("single-group ledger permission=%v err=%v", ok, err)
	}
	if ok, err := b.canInvite(ctx, replyTargetID); err != nil || ok {
		t.Fatalf("single-group operator invite permission=%v err=%v", ok, err)
	}
	if ok, err := b.canUseBroadcastFresh(ctx, replyTargetID); err != nil || ok {
		t.Fatalf("single-group operator private broadcast permission=%v err=%v", ok, err)
	}
}
