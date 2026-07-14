package bot

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestSplitRouteKeepsLedgerStateAndReadsInOneFIFO(t *testing.T) {
	b := New(config.Config{GroupRouteMode: "split", LedgerWorkers: 2, QueryWorkers: 2, NotifyWorkers: 2, QueueSize: 32}, nil, nil, nil, nil)
	chatID := int64(-100123)
	message := func(id int64, text string) telegram.Update {
		return telegram.Update{UpdateID: id, Message: &telegram.Message{
			MessageID: id, Chat: telegram.Chat{ID: chatID, Type: "supergroup"}, Text: text,
		}}
	}
	ledgerCommands := []string{
		"+100", "-100", "下发100U", "撤销", "开始", "停止", "设置汇率10", "设置费率0",
		"设置日切4", "清除当前账期", "添加操作员", "+0", "显示账单",
		"TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ",
	}
	for i, command := range ledgerCommands {
		key, pool := b.updateRoute(message(int64(i+1), command))
		if key != "ledger:-100123" || pool != b.ledgerPool {
			t.Fatalf("%q route = %s/%p, want ledger FIFO", command, key, pool)
		}
	}
	for i, command := range []string{"普通聊天", "1+2", "Z0", ""} {
		update := message(int64(100+i), command)
		key, pool := b.updateRoute(update)
		if key != fmt.Sprintf("bypass:%d:%d", chatID, update.UpdateID) || pool != b.queryPool {
			t.Fatalf("%q route = %s/%p, want independent bypass", command, key, pool)
		}
	}
	callback := telegram.Update{UpdateID: 200, CallbackQuery: &telegram.CallbackQuery{Message: &telegram.Message{Chat: telegram.Chat{ID: chatID, Type: "supergroup"}}}}
	if key, pool := b.updateRoute(callback); key != "ledger:-100123" || pool != b.ledgerPool {
		t.Fatalf("ledger callback route = %s/%p", key, pool)
	}
	membership := telegram.Update{UpdateID: 201, MyChatMember: &telegram.ChatMemberUpd{Chat: telegram.Chat{ID: chatID, Type: "supergroup"}}}
	if key, pool := b.updateRoute(membership); key != "ledger:-100123" || pool != b.ledgerPool {
		t.Fatalf("membership route = %s/%p", key, pool)
	}
}

func TestSplitRouteOrdinaryFloodDoesNotBlockLedgerFIFO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := New(config.Config{GroupRouteMode: "split", LedgerWorkers: 4, QueryWorkers: 4, NotifyWorkers: 2, QueueSize: 2048}, nil, nil, nil, nil)
	b.ledgerPool.Start(ctx)
	b.queryPool.Start(ctx)
	release := make(chan struct{})
	defer close(release)
	for i := 0; i < 1000; i++ {
		update := telegram.Update{UpdateID: int64(i + 1), Message: &telegram.Message{Chat: telegram.Chat{ID: -1001, Type: "group"}, Text: "普通聊天"}}
		key, pool := b.updateRoute(update)
		b.dispatcher.Submit(ctx, key, pool, func(jobCtx context.Context) {
			select {
			case <-release:
			case <-jobCtx.Done():
			}
		})
	}
	ledgerDone := make(chan struct{})
	update := telegram.Update{UpdateID: 2000, Message: &telegram.Message{Chat: telegram.Chat{ID: -1001, Type: "group"}, Text: "+100"}}
	key, pool := b.updateRoute(update)
	b.dispatcher.Submit(ctx, key, pool, func(context.Context) { close(ledgerDone) })
	select {
	case <-ledgerDone:
	case <-time.After(time.Second):
		t.Fatal("ordinary-message flood blocked the ledger FIFO")
	}

	order := make(chan int, 3)
	for i := 1; i <= 3; i++ {
		value := i
		b.dispatcher.Submit(ctx, "ledger:-2002", b.ledgerPool, func(context.Context) { order <- value })
	}
	for want := 1; want <= 3; want++ {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("same-chat ledger order = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("same-chat ledger FIFO timed out")
		}
	}
	blockChat := make(chan struct{})
	b.dispatcher.Submit(ctx, "ledger:-3001", b.ledgerPool, func(jobCtx context.Context) {
		select {
		case <-blockChat:
		case <-jobCtx.Done():
		}
	})
	otherChatDone := make(chan struct{})
	b.dispatcher.Submit(ctx, "ledger:-3002", b.ledgerPool, func(context.Context) { close(otherChatDone) })
	select {
	case <-otherChatDone:
		close(blockChat)
	case <-time.After(time.Second):
		close(blockChat)
		t.Fatal("one chat blocked a different chat ledger FIFO")
	}
}

func TestLegacyRouteGateRestoresSingleChatFIFO(t *testing.T) {
	b := New(config.Config{GroupRouteMode: "legacy", LedgerWorkers: 1, QueryWorkers: 1, NotifyWorkers: 2, QueueSize: 32}, nil, nil, nil, nil)
	update := telegram.Update{UpdateID: 1, Message: &telegram.Message{Chat: telegram.Chat{ID: -9, Type: "group"}, Text: "普通聊天"}}
	if key, pool := b.updateRoute(update); key != "ledger:-9" || pool != b.ledgerPool {
		t.Fatalf("legacy route = %s/%p", key, pool)
	}
}
