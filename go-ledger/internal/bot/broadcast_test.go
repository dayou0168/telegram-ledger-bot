package bot

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

func TestFormatBroadcastResultText(t *testing.T) {
	if got := formatBroadcastResultText(1, 0); got != "发送完成" {
		t.Fatalf("single success = %q", got)
	}
	if got := formatBroadcastResultText(20, 0); got != "发送完成" {
		t.Fatalf("broadcast success = %q", got)
	}
	if got := formatBroadcastResultText(2, 1); got != "部分发送失败：失败 1 个。" {
		t.Fatalf("partial result = %q", got)
	}
	if got := formatBroadcastResultText(0, 1); got != "发送失败，请稍后重试。" {
		t.Fatalf("failure result = %q", got)
	}
}

func TestBroadcastSessionControls(t *testing.T) {
	if !isBroadcastNotifyToggleText("通知所有人：关") || !isBroadcastNotifyToggleText("通知所有人:开") {
		t.Fatal("notify toggle text was not recognized")
	}
	if isBroadcastNotifyToggleText("通知所有人：今天发货") {
		t.Fatal("ordinary content should not be treated as notify toggle")
	}
	if !isBroadcastEndText("结束广播") || !isBroadcastEndText("取消广播") {
		t.Fatal("end broadcast text was not recognized")
	}
	if !isBroadcastSwitchTargetText("切换群") || !isBroadcastSwitchTargetText("切换目标") {
		t.Fatal("switch target text was not recognized")
	}
	if !isBroadcastMenuText("切换群") {
		t.Fatal("switch target should open broadcast target menu outside a session")
	}

	keyboard := broadcastSessionKeyboard("11", true)
	if len(keyboard.Keyboard) != 2 || len(keyboard.Keyboard[0]) != 1 || len(keyboard.Keyboard[1]) != 3 {
		t.Fatalf("unexpected keyboard shape: %#v", keyboard.Keyboard)
	}
	if !keyboard.IsPersistent {
		t.Fatal("broadcast session keyboard should be persistent")
	}
	if keyboard.Keyboard[0][0].Text != "当前目标：11" {
		t.Fatalf("unexpected target label: %#v", keyboard.Keyboard[0])
	}
	if keyboard.Keyboard[1][0].Text != "通知所有人：开" || keyboard.Keyboard[1][1].Text != "切换群" || keyboard.Keyboard[1][2].Text != "结束广播" {
		t.Fatalf("unexpected keyboard labels: %#v", keyboard.Keyboard[1])
	}
}

func TestQuickReplyControlsAndExitState(t *testing.T) {
	keyboard := quickReplyKeyboard("出款群", true)
	if len(keyboard.Keyboard) != 2 || len(keyboard.Keyboard[1]) != 3 {
		t.Fatalf("unexpected quick reply keyboard shape: %#v", keyboard.Keyboard)
	}
	if keyboard.Keyboard[0][0].Text != "当前快速回复：出款群" {
		t.Fatalf("unexpected quick reply target label: %#v", keyboard.Keyboard[0])
	}
	if keyboard.Keyboard[1][0].Text != "结束快速回复" || keyboard.Keyboard[1][1].Text != "返回广播" || keyboard.Keyboard[1][2].Text != "取消" {
		t.Fatalf("unexpected quick reply exit controls: %#v", keyboard.Keyboard[1])
	}
	if !keyboard.IsPersistent {
		t.Fatal("quick reply keyboard should be persistent")
	}
	if !isQuickReplyEndText("结束快速回复") || !isQuickReplyEndText("返回广播") || !isQuickReplyEndText("取消") {
		t.Fatal("quick reply exit text was not recognized")
	}
	if !isQuickReplyStatusText("当前快速回复：出款群") {
		t.Fatal("quick reply status label was not recognized")
	}

	restored, ok := quickReplyReturnState(privateState{
		Mode:                   "quick_reply",
		ReturnMode:             "broadcast",
		ReturnTargetName:       "出款",
		ReturnChatIDs:          []int64{-1001, -1002},
		ReturnNotifyAll:        true,
		ReturnControlMessageID: 42,
	})
	if !ok {
		t.Fatal("quick reply should restore the previous broadcast state")
	}
	if restored.Mode == "quick_reply" || restored.Mode != "broadcast" {
		t.Fatalf("quick reply exit should leave quick_reply mode, got %q", restored.Mode)
	}
	if restored.TargetName != "出款" || len(restored.ChatIDs) != 2 || !restored.NotifyAll || restored.ControlMessageID != 42 {
		t.Fatalf("unexpected restored state: %+v", restored)
	}
	restored.ChatIDs[0] = 999
	if restored.ReturnChatIDs != nil {
		t.Fatalf("restored broadcast state should not keep quick reply return fields: %+v", restored)
	}

	if _, ok := quickReplyReturnState(privateState{Mode: "quick_reply"}); ok {
		t.Fatal("quick reply without return state should fall back to menu")
	}
}

func TestBroadcastPickerPagesPastFortyChats(t *testing.T) {
	items := make([]storage.Group, 65)
	for i := range items {
		items[i] = storage.Group{ChatID: int64(-1000 - i), Title: fmt.Sprintf("群 %02d", i+1)}
	}
	page, start, end, pages := pickerBounds(len(items), 4)
	if start != 48 || end != 60 || page != 4 || pages != 6 {
		t.Fatalf("picker page = start %d end %d page %d pages %d", start, end, page, pages)
	}
	page, start, end, pages = pickerBounds(len(items), 99)
	if start != 60 || end != 65 || page != 5 || pages != 6 {
		t.Fatalf("clamped picker page = start %d end %d page %d pages %d", start, end, page, pages)
	}
}

func TestIntersectBroadcastTargetsRechecksAndKeepsStoredTitle(t *testing.T) {
	allowed := []storage.Group{{ChatID: -1002, Title: "当前群名"}, {ChatID: -1004, Title: "新增群"}}
	got := intersectBroadcastTargets([]int64{-1001, -1002, -1003}, allowed)
	if len(got) != 1 || got[0].ChatID != -1002 || got[0].Title != "当前群名" {
		t.Fatalf("dynamic targets = %+v", got)
	}
}

func TestBroadcastReplyKeyboardHidesQuickReplyWithoutPermission(t *testing.T) {
	msg := telegram.Message{Chat: telegram.Chat{ID: -1001}, MessageID: 20}
	chatDelivery := storage.BroadcastDelivery{ID: 7, Mode: "chat", TargetChatID: -1001, TargetMessageID: 10}
	viewer := broadcastReplyKeyboard(msg, chatDelivery, false)
	if len(viewer.InlineKeyboard) != 0 {
		t.Fatalf("single-chat viewer keyboard rows = %#v", viewer.InlineKeyboard)
	}
	operator := broadcastReplyKeyboard(msg, chatDelivery, true)
	if len(operator.InlineKeyboard) != 1 || operator.InlineKeyboard[0][0].CallbackData != "br:q:7" {
		t.Fatalf("single-chat operator keyboard rows = %#v", operator.InlineKeyboard)
	}
	for _, mode := range []string{"group", "all"} {
		delivery := chatDelivery
		delivery.Mode = mode
		viewer = broadcastReplyKeyboard(msg, delivery, false)
		if len(viewer.InlineKeyboard) != 1 || len(viewer.InlineKeyboard[0]) != 2 {
			t.Fatalf("%s viewer location rows = %#v", mode, viewer.InlineKeyboard)
		}
		operator = broadcastReplyKeyboard(msg, delivery, true)
		if len(operator.InlineKeyboard) != 2 || operator.InlineKeyboard[0][0].CallbackData != "br:q:7" {
			t.Fatalf("%s operator keyboard rows = %#v", mode, operator.InlineKeyboard)
		}
	}
}

func TestBroadcastReplyKeyboardModeIsIndependentOfReplyMedia(t *testing.T) {
	for _, msg := range []telegram.Message{
		{Text: "reply", Chat: telegram.Chat{ID: -1001}, MessageID: 20},
		{Photo: []telegram.Photo{{FileID: "photo"}}, Chat: telegram.Chat{ID: -1001}, MessageID: 20},
		{Photo: []telegram.Photo{{FileID: "photo"}}, Caption: "caption", Chat: telegram.Chat{ID: -1001}, MessageID: 20},
	} {
		chat := broadcastReplyKeyboard(msg, storage.BroadcastDelivery{ID: 1, Mode: "chat"}, false)
		group := broadcastReplyKeyboard(msg, storage.BroadcastDelivery{ID: 2, Mode: "group"}, false)
		if len(chat.InlineKeyboard) != 0 || len(group.InlineKeyboard) != 1 {
			t.Fatalf("media=%+v chat=%+v group=%+v", msg, chat, group)
		}
	}
}

func TestBroadcastReliablePayloadMatrixPreservesTextPhotoAndCaption(t *testing.T) {
	tests := []struct {
		name                            string
		msg                             telegram.Message
		typeName, text, fileID, caption string
	}{
		{name: "text", msg: telegram.Message{Text: "hello"}, typeName: "text", text: "hello"},
		{name: "photo", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "small"}, {FileID: "large"}}}, typeName: "photo", fileID: "large"},
		{name: "photo caption", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "photo"}}, Caption: "caption"}, typeName: "photo", fileID: "photo", caption: "caption"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, ok := reliablePayloadFromMessage(test.msg)
			if !ok || payload.Type != test.typeName || payload.Text != test.text || payload.FileID != test.fileID || payload.Caption != test.caption {
				t.Fatalf("payload=%+v ok=%t", payload, ok)
			}
		})
	}
	for _, test := range []struct {
		name, wantType, wantContent string
		msg                         telegram.Message
	}{
		{name: "reply text", msg: telegram.Message{Text: "text reply"}, wantType: "text", wantContent: "text reply"},
		{name: "reply photo", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "reply"}}}, wantType: "photo", wantContent: "[图片]"},
		{name: "reply photo caption", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "reply"}}, Caption: "photo reply"}, wantType: "photo", wantContent: "photo reply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, ok := broadcastReplyPayload(test.msg, storage.User{ID: 9, DisplayName: "user"}, storage.BroadcastDelivery{TargetTitle: "group"}, "@operator")
			content := payload.Text
			if payload.Type == "photo" {
				content = payload.Caption
			}
			if !ok || payload.Type != test.wantType || !strings.Contains(content, test.wantContent) || !strings.Contains(content, "原广播发送人：@operator") {
				t.Fatalf("reply payload=%+v ok=%t", payload, ok)
			}
		})
	}
}

func TestBroadcastUpstreamPayloadsShowContextAndPreserveMedia(t *testing.T) {
	dispatch := broadcastDispatchContext{SenderLabel: "@operator", TargetDisplay: "广播分组 · 财务组（当前 3 个群）"}
	for _, test := range []struct {
		name    string
		msg     telegram.Message
		kind    string
		fileID  string
		content string
	}{
		{name: "text", msg: telegram.Message{Text: "原始文字"}, kind: "text", content: "原始内容：\n原始文字"},
		{name: "photo", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "photo"}}}, kind: "photo", fileID: "photo"},
		{name: "photo caption", msg: telegram.Message{Photo: []telegram.Photo{{FileID: "photo-caption"}}, Caption: "原始说明"}, kind: "photo", fileID: "photo-caption", content: "原始说明：\n原始说明"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, companion, ok := broadcastUpstreamPayloads(test.msg, dispatch)
			if !ok || companion != nil || payload.Type != test.kind || payload.FileID != test.fileID {
				t.Fatalf("payload=%+v companion=%+v ok=%t", payload, companion, ok)
			}
			visible := payload.Text
			if payload.Type == "photo" {
				visible = payload.Caption
			}
			if !strings.Contains(visible, "发送人：@operator") || !strings.Contains(visible, "发送目标：广播分组 · 财务组（当前 3 个群）") || !strings.Contains(visible, test.content) {
				t.Fatalf("visible content=%q", visible)
			}
		})
	}
}

func TestBroadcastUpstreamPayloadsKeepLongOriginalAndUseCompanion(t *testing.T) {
	dispatch := broadcastDispatchContext{SenderLabel: "操作人", TargetDisplay: "单群 · 当前群名"}
	longCaption := strings.Repeat("图", telegramCaptionLimitUnits+1)
	payload, companion, ok := broadcastUpstreamPayloads(telegram.Message{
		Photo: []telegram.Photo{{FileID: "original-photo"}}, Caption: longCaption,
	}, dispatch)
	if !ok || payload.Type != "photo" || payload.FileID != "original-photo" || payload.Caption != longCaption {
		t.Fatalf("long media payload=%+v ok=%t", payload, ok)
	}
	if companion == nil || companion.Type != "text" || !strings.Contains(companion.Text, "发送人：操作人") || !strings.Contains(companion.Text, "发送目标：单群 · 当前群名") {
		t.Fatalf("companion=%+v", companion)
	}
	longText := strings.Repeat("😀", telegramTextLimitUnits/2+1)
	payload, companion, ok = broadcastUpstreamPayloads(telegram.Message{Text: longText}, dispatch)
	if !ok || payload.Text != longText || companion == nil || telegramTextUnits(longText) <= telegramTextLimitUnits {
		t.Fatalf("long text payload=%+v companion=%+v units=%d", payload, companion, telegramTextUnits(longText))
	}
}

func TestBroadcastUpstreamCaptionBoundary(t *testing.T) {
	dispatch := broadcastDispatchContext{SenderLabel: "@sender", TargetDisplay: "单群 · 群名"}
	prefix := "发送人：@sender\n发送目标：单群 · 群名\n\n原始说明：\n"
	exactCaption := strings.Repeat("a", telegramCaptionLimitUnits-telegramTextUnits(prefix))
	payload, companion, ok := broadcastUpstreamPayloads(telegram.Message{Photo: []telegram.Photo{{FileID: "photo"}}, Caption: exactCaption}, dispatch)
	if !ok || companion != nil || telegramTextUnits(payload.Caption) != telegramCaptionLimitUnits {
		t.Fatalf("exact payload units=%d companion=%+v ok=%t", telegramTextUnits(payload.Caption), companion, ok)
	}
	payload, companion, ok = broadcastUpstreamPayloads(telegram.Message{Photo: []telegram.Photo{{FileID: "photo"}}, Caption: exactCaption + "b"}, dispatch)
	if !ok || companion == nil || payload.Caption != exactCaption+"b" {
		t.Fatalf("overflow payload=%+v companion=%+v ok=%t", payload, companion, ok)
	}
}

func TestBroadcastSenderLabelPrefersUsernameThenDisplayName(t *testing.T) {
	b := &Bot{}
	if got := b.broadcastSenderLabel(context.Background(), storage.User{ID: 1, Username: "alice", DisplayName: "Alice"}); got != "@alice" {
		t.Fatalf("username label=%q", got)
	}
	if got := b.broadcastSenderLabel(context.Background(), storage.User{ID: 2, DisplayName: "Alice Example"}); got != "Alice Example" {
		t.Fatalf("display label=%q", got)
	}
}

func TestPrivateStateTTLBoundaryDoesNotExpireDurableBroadcastTarget(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	if !privateStateIsFresh(now.Add(-29*time.Minute-59*time.Second), now) {
		t.Fatal("29:59 transient state should remain fresh")
	}
	if privateStateIsFresh(now.Add(-30*time.Minute), now) || privateStateIsFresh(now.Add(-30*time.Minute-time.Second), now) {
		t.Fatal("30:00 and 30:01 transient states should expire")
	}
	state := privateStateFromBroadcastTarget(storage.TelegramBroadcastTarget{Mode: "group", GroupID: 7, TargetName: "saved", NotifyAll: true, UpdatedAt: now.Add(-365 * 24 * time.Hour)})
	if !isBroadcastTargetState(state) || state.GroupID != 7 || !state.NotifyAll {
		t.Fatalf("durable target should not depend on transient ttl: %+v", state)
	}
}

func TestPostgresBroadcastReplyRecipientMatrixAndPreferences(t *testing.T) {
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

	base := int64(985000000000 + time.Now().UnixNano()%1000000)
	hostID, defaultAID, defaultBID := base, base+1, base+2
	primaryAID, primaryBID := base+3, base+4
	secondaryAID, secondaryBID := base+5, base+6
	now := time.Now().UTC()
	for _, op := range []struct {
		id, parent int64
		level      string
	}{
		{primaryAID, 0, "primary"},
		{primaryBID, 0, "primary"},
		{secondaryAID, primaryAID, "secondary"},
		{secondaryBID, primaryBID, "secondary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.id, op.level, op.parent, hostID, "reply matrix", now); err != nil {
			t.Fatal(err)
		}
	}
	b := &Bot{store: store, perms: permissions.NewPolicy(hostID, map[int64]struct{}{defaultAID: {}, defaultBID: {}})}

	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, secondaryAID),
		secondaryAID, hostID, primaryAID)
	if changed, err := store.UpsertOperatorMessageObserverGrant(ctx, secondaryAID, primaryBID, false, true, hostID, now); err != nil || !changed {
		t.Fatalf("grant reply observer changed=%v err=%v", changed, err)
	}
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, secondaryAID),
		secondaryAID, hostID, primaryAID, primaryBID)
	if err := store.SetBroadcastReplyPreference(ctx, hostID, secondaryAID, false, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBroadcastReplyPreference(ctx, primaryAID, secondaryAID, false, now); err != nil {
		t.Fatal(err)
	}
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, secondaryAID), secondaryAID, primaryBID)

	if err := store.SetBroadcastReplyPreference(ctx, hostID, secondaryAID, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, secondaryAID), secondaryAID, hostID, primaryBID)
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, primaryAID), primaryAID, hostID)
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, defaultAID), defaultAID)

	brokenStore, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	brokenStore.Close()
	brokenBot := &Bot{store: brokenStore, perms: b.perms}
	assertBroadcastReplyRecipients(t, brokenBot.broadcastReplyRecipients(ctx, secondaryAID), secondaryAID)
}

func TestPostgresBroadcastUpstreamHierarchyAndObserverGrants(t *testing.T) {
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
	base := int64(986000000000 + time.Now().UnixNano()%1000000)
	hostID, primaryAID, primaryBID, secondaryAID := base, base+1, base+2, base+3
	now := time.Now().UTC()
	for _, op := range []struct {
		id, parent int64
		level      string
	}{{primaryAID, 0, "primary"}, {primaryBID, 0, "primary"}, {secondaryAID, primaryAID, "secondary"}} {
		if err := store.UpsertGlobalOperator(ctx, op.id, op.level, op.parent, hostID, "upstream hierarchy", now); err != nil {
			t.Fatal(err)
		}
	}
	b := &Bot{store: store, perms: permissions.NewPolicy(hostID, nil), loc: time.UTC, privateStates: newTTLCache[privateState](privateStateTTL)}
	if got := b.broadcastSenderLabel(ctx, storage.User{ID: secondaryAID}); got != "upstream hierarchy" {
		t.Fatalf("operator remark fallback label=%q", got)
	}
	assertInt64Slice(t, mustBroadcastUpstreamRecipients(t, b, ctx, hostID))
	assertInt64Slice(t, mustBroadcastUpstreamRecipients(t, b, ctx, primaryAID), hostID)
	assertInt64Slice(t, mustBroadcastUpstreamRecipients(t, b, ctx, secondaryAID), hostID, primaryAID)
	if changed, err := store.UpsertOperatorMessageObserverGrant(ctx, secondaryAID, primaryBID, true, false, hostID, now); err != nil || !changed {
		t.Fatalf("grant broadcast observer changed=%v err=%v", changed, err)
	}
	assertInt64Slice(t, mustBroadcastUpstreamRecipients(t, b, ctx, secondaryAID), hostID, primaryAID, primaryBID)
	if err := store.SetBroadcastMessagePreference(ctx, hostID, secondaryAID, false, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	assertInt64Slice(t, mustBroadcastUpstreamRecipients(t, b, ctx, secondaryAID), primaryAID, primaryBID)
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, secondaryAID), secondaryAID, hostID, primaryAID)
	if err := store.SetBroadcastMessagePreference(ctx, primaryBID, secondaryAID, false, false, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	assertInt64Slice(t, mustBroadcastUpstreamRecipients(t, b, ctx, secondaryAID), primaryAID)
	assertBroadcastReplyRecipients(t, b.broadcastReplyRecipients(ctx, secondaryAID), secondaryAID, hostID, primaryAID)
	if err := b.saveBroadcastTarget(ctx, secondaryAID, privateState{Mode: "chat", TargetChatID: -1001, TargetName: "target"}); err != nil {
		t.Fatal(err)
	}
	b.privateStates.Set(formatID(secondaryAID), privateState{Mode: "chat", TargetChatID: -1001})
	b.InvalidateBroadcastPermission(secondaryAID)
	if _, ok, err := store.GetTelegramBroadcastTarget(ctx, b.telegramInboxStreamKey(), secondaryAID); err != nil || ok {
		t.Fatalf("revoked target remained ok=%t err=%v", ok, err)
	}
}

func TestPostgresBroadcastReplyQuickReplyEligibilityTracksRecipientsAndRestart(t *testing.T) {
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
	base := int64(987000000000 + time.Now().UnixNano()%1000000)
	hostID, directPrimaryID, observerPrimaryID, noPermissionObserverID, sourceID := base, base+1, base+2, base+3, base+4
	targetChatID := -base - 10
	now := time.Now().UTC()
	for _, op := range []struct {
		id, parent int64
		level      string
	}{
		{directPrimaryID, 0, "primary"},
		{observerPrimaryID, 0, "primary"},
		{noPermissionObserverID, 0, "primary"},
		{sourceID, directPrimaryID, "secondary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.id, op.level, op.parent, hostID, "quick reply role", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnsureGroup(ctx, targetChatID, "quick reply group", now); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{sourceID, directPrimaryID, observerPrimaryID} {
		if err := store.AddBroadcastPermission(ctx, userID, "chat", targetChatID, "", hostID, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, observerID := range []int64{observerPrimaryID, noPermissionObserverID} {
		if _, err := store.UpsertOperatorMessageObserverGrant(ctx, sourceID, observerID, false, true, hostID, now); err != nil {
			t.Fatal(err)
		}
	}
	deliveryID, err := store.InsertBroadcastDelivery(ctx, storage.BroadcastDelivery{
		OperatorUserID: sourceID, SourceChatID: sourceID, SourceMessageID: base + 20,
		TargetChatID: targetChatID, TargetTitle: "quick reply group", TargetMessageID: base + 21,
		Mode: "group", TargetName: "group", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	delivery, ok, err := store.GetBroadcastDelivery(ctx, deliveryID)
	if err != nil || !ok {
		t.Fatalf("delivery=%+v ok=%t err=%v", delivery, ok, err)
	}
	policy := permissions.NewPolicy(hostID, nil)
	b := &Bot{store: store, perms: policy}
	for _, userID := range []int64{hostID, sourceID, directPrimaryID, observerPrimaryID, noPermissionObserverID} {
		if allowed, err := b.canQuickReplyDeliveryFresh(ctx, userID, delivery); err != nil || !allowed {
			t.Fatalf("user %d quick reply allowed=%t err=%v", userID, allowed, err)
		}
	}
	if err := store.SetBroadcastMessagePreference(ctx, observerPrimaryID, sourceID, true, false, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if allowed, err := b.canQuickReplyDeliveryFresh(ctx, observerPrimaryID, delivery); err != nil || allowed {
		t.Fatalf("observer with reply preference disabled allowed=%t err=%v", allowed, err)
	}
	if allowed, err := b.canQuickReplyDeliveryFresh(ctx, noPermissionObserverID, delivery); err != nil || !allowed {
		t.Fatalf("independent observer without group membership allowed=%t err=%v", allowed, err)
	}
	if changed, err := store.RevokeOperatorMessageObserverGrant(ctx, sourceID, noPermissionObserverID, hostID, now.Add(2*time.Second)); err != nil || !changed {
		t.Fatalf("revoke observer grant changed=%t err=%v", changed, err)
	}
	if allowed, err := b.canQuickReplyDeliveryFresh(ctx, noPermissionObserverID, delivery); err != nil || allowed {
		t.Fatalf("observer after grant revoke allowed=%t err=%v", allowed, err)
	}
	store.Close()
	restartedStore, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer restartedStore.Close()
	restarted := &Bot{store: restartedStore, perms: policy}
	delivery, ok, err = restartedStore.GetBroadcastDelivery(ctx, deliveryID)
	if err != nil || !ok || !restarted.canQuickReplyDelivery(ctx, hostID, delivery) || !restarted.canQuickReplyDelivery(ctx, directPrimaryID, delivery) || restarted.canQuickReplyDelivery(ctx, observerPrimaryID, delivery) {
		t.Fatalf("restart delivery=%+v ok=%t host=%t direct=%t observer=%t err=%v", delivery, ok,
			restarted.canQuickReplyDelivery(ctx, hostID, delivery), restarted.canQuickReplyDelivery(ctx, directPrimaryID, delivery), restarted.canQuickReplyDelivery(ctx, observerPrimaryID, delivery), err)
	}
}

func TestPostgresBroadcastDispatchTargetUsesCurrentChatAndGroupNames(t *testing.T) {
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
	base := int64(988000000000 + time.Now().UnixNano()%1000000)
	hostID, chatA, chatB := base, -base-10, -base-11
	now := time.Now().UTC()
	if err := store.EnsureGroup(ctx, chatA, "旧群名", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatB, "第二群", now); err != nil {
		t.Fatal(err)
	}
	groupName := fmt.Sprintf("当前分组-%d", base)
	if err := store.UpsertBroadcastGroup(ctx, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddChatsToBroadcastGroup(ctx, groupName, []int64{chatA}, now); err != nil {
		t.Fatal(err)
	}
	group, ok, err := store.GetBroadcastGroup(ctx, groupName)
	if err != nil || !ok {
		t.Fatalf("group=%+v ok=%t err=%v", group, ok, err)
	}
	b := &Bot{store: store, perms: permissions.NewPolicy(hostID, nil), loc: time.UTC}
	if err := b.saveBroadcastTarget(ctx, hostID, privateState{Mode: "group", GroupID: group.ID, TargetName: groupName}); err != nil {
		t.Fatal(err)
	}
	chatTargets, chatName, chatDisplay, err := b.resolveCurrentBroadcastTarget(ctx, hostID, privateState{Mode: "chat", TargetChatID: chatA, TargetName: "过期群名"})
	if err != nil || len(chatTargets) != 1 || chatName != "旧群名" || chatDisplay != "单群 · 旧群名" {
		t.Fatalf("chat targets=%+v name=%q display=%q err=%v", chatTargets, chatName, chatDisplay, err)
	}
	if err := store.EnsureGroup(ctx, chatA, "新群名", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, chatName, chatDisplay, err = b.resolveCurrentBroadcastTarget(ctx, hostID, privateState{Mode: "chat", TargetChatID: chatA, TargetName: "旧群名"})
	if err != nil || chatName != "新群名" || chatDisplay != "单群 · 新群名" {
		t.Fatalf("renamed chat name=%q display=%q err=%v", chatName, chatDisplay, err)
	}
	if _, err := store.AddChatsToBroadcastGroup(ctx, groupName, []int64{chatB}, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	renamed := groupName + "-renamed"
	if changed, _, err := store.RenameBroadcastGroup(ctx, groupName, renamed, hostID, true, now.Add(3*time.Second)); err != nil || !changed {
		t.Fatalf("rename changed=%t err=%v", changed, err)
	}
	restoredGroupState, ok, err := b.loadBroadcastTargetState(ctx, hostID)
	if err != nil || !ok {
		t.Fatalf("restored renamed group state=%+v ok=%t err=%v", restoredGroupState, ok, err)
	}
	groupTargets, currentGroupName, groupDisplay, err := b.resolveCurrentBroadcastTarget(ctx, hostID, restoredGroupState)
	if err != nil || len(groupTargets) != 2 || currentGroupName != renamed || !strings.Contains(groupDisplay, renamed) || !strings.Contains(groupDisplay, "当前 2 个群") {
		t.Fatalf("group targets=%+v name=%q display=%q err=%v", groupTargets, currentGroupName, groupDisplay, err)
	}
	allTargets, allName, allDisplay, err := b.resolveCurrentBroadcastTarget(ctx, hostID, privateState{Mode: "all"})
	if err != nil || len(allTargets) < 2 || allName != "全部授权群" || !strings.Contains(allDisplay, fmt.Sprintf("当前 %d 个群", len(allTargets))) {
		t.Fatalf("all targets=%d name=%q display=%q err=%v", len(allTargets), allName, allDisplay, err)
	}
}

func mustBroadcastUpstreamRecipients(t *testing.T, b *Bot, ctx context.Context, sourceID int64) []int64 {
	t.Helper()
	ids, err := b.broadcastUpstreamRecipients(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func assertInt64Slice(t *testing.T, got []int64, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

func assertBroadcastReplyRecipients(t *testing.T, got map[int64]struct{}, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("recipients=%v want=%v", sortedRecipientIDs(got), want)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("recipients=%v missing=%d", sortedRecipientIDs(got), id)
		}
	}
}

func sortedRecipientIDs(values map[int64]struct{}) []int64 {
	ids := make([]int64, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func TestBroadcastQueueFullRunsFailureFallback(t *testing.T) {
	pool := worker.NewPool("broadcast-test", 1, 1)
	for pool.Submit(func(context.Context) {}) {
	}
	called := false
	submitted, err := submitBroadcastJob(pool, func(context.Context) {}, func() error {
		called = true
		return nil
	})
	if err != nil || submitted || !called {
		t.Fatalf("queue-full submitted=%t fallback called=%t err=%v", submitted, called, err)
	}
}

func TestPostgresBroadcastGroupVisibilityAndSendRecheck(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := int64(970000000000 + now.UnixNano()%1000000)
	hostID := base
	ownerID := base + 1
	peerID := base + 2
	chatID := -base
	for _, userID := range []int64{ownerID, peerID} {
		if err := store.UpsertGlobalOperator(ctx, userID, "primary", 0, hostID, "bot fixture", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.EnsureGroup(ctx, chatID, "bot scope", now); err != nil {
		t.Fatal(err)
	}
	for _, userID := range []int64{ownerID, peerID} {
		if err := store.AddBroadcastPermission(ctx, userID, "chat", chatID, "", hostID, now); err != nil {
			t.Fatal(err)
		}
	}
	groupName := fmt.Sprintf("bot-owned-%d", base)
	if created, err := store.CreateBroadcastGroup(ctx, groupName, ownerID, ownerID, now); err != nil || !created {
		t.Fatalf("create group=%v err=%v", created, err)
	}
	if added, err := store.AddChatsToBroadcastGroupManaged(ctx, groupName, []int64{chatID}, ownerID, false, now); err != nil || added != 1 {
		t.Fatalf("add member=%d err=%v", added, err)
	}
	b := &Bot{store: store, perms: permissions.NewPolicy(hostID, nil)}

	ownerOptions, err := b.allowedBroadcastGroupOptions(ctx, ownerID)
	if err != nil || len(ownerOptions) != 1 || ownerOptions[0].Name != groupName {
		t.Fatalf("owner options=%+v err=%v", ownerOptions, err)
	}
	peerOptions, err := b.allowedBroadcastGroupOptions(ctx, peerID)
	if err != nil || len(peerOptions) != 0 {
		t.Fatalf("chat overlap leaked group: options=%+v err=%v", peerOptions, err)
	}
	if err := store.AddBroadcastPermission(ctx, peerID, "group", 0, groupName, ownerID, now); err != nil {
		t.Fatal(err)
	}
	peerOptions, err = b.allowedBroadcastGroupOptions(ctx, peerID)
	if err != nil || len(peerOptions) != 1 || peerOptions[0].Name != groupName {
		t.Fatalf("authorized peer options=%+v err=%v", peerOptions, err)
	}
	state := privateState{Mode: "group", TargetName: groupName, ChatIDs: []int64{chatID}}
	if targets, err := b.currentBroadcastTargets(ctx, peerID, state); err != nil || len(targets) != 1 {
		t.Fatalf("authorized send targets=%+v err=%v", targets, err)
	}
	if removed, err := store.RemoveBroadcastPermission(ctx, peerID, "group", 0, groupName, ownerID, now.Add(time.Second)); err != nil || !removed {
		t.Fatalf("revoke group=%v err=%v", removed, err)
	}
	if targets, err := b.currentBroadcastTargets(ctx, peerID, state); err != nil || len(targets) != 0 {
		t.Fatalf("send recheck retained revoked target=%+v err=%v", targets, err)
	}
}
