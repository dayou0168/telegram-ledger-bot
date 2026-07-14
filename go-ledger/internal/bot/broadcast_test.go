package bot

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

func TestFormatBroadcastProgressText(t *testing.T) {
	if got := formatBroadcastProgressText("chat", 1); got != "发送中..." {
		t.Fatalf("single chat progress = %q", got)
	}
	if got := formatBroadcastProgressText("group", 3); got != "广播发送中：目标 3 个。" {
		t.Fatalf("group progress = %q", got)
	}
}

func TestFormatBroadcastResultText(t *testing.T) {
	if got := formatBroadcastResultText("chat", 1, 0, false); got != "发送完成。" {
		t.Fatalf("single chat success = %q", got)
	}
	if got := formatBroadcastResultText("chat", 0, 1, true); got != "发送失败。\n通知所有人：开启" {
		t.Fatalf("single chat failed notify = %q", got)
	}
	if got := formatBroadcastResultText("group", 2, 1, false); got != "广播完成：成功 2 个，失败 1 个。" {
		t.Fatalf("group result = %q", got)
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
	delivery := storage.BroadcastDelivery{ID: 7, TargetChatID: -1001, TargetMessageID: 10}
	viewer := broadcastReplyKeyboard(msg, delivery, false)
	if len(viewer.InlineKeyboard) != 1 {
		t.Fatalf("viewer keyboard rows = %#v", viewer.InlineKeyboard)
	}
	operator := broadcastReplyKeyboard(msg, delivery, true)
	if len(operator.InlineKeyboard) != 2 || operator.InlineKeyboard[0][0].CallbackData != "br:q:7" {
		t.Fatalf("operator keyboard rows = %#v", operator.InlineKeyboard)
	}
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
