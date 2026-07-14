package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

func TestPostgresClearLedgerCallbackRechecksPermissionAndPeriod(t *testing.T) {
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

	answers := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		answers <- strings.TrimPrefix(r.URL.Path, "/bottest/") + ":" + body.Text
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	cfg := config.Config{
		TelegramAPIBase:              server.URL,
		Timezone:                     "Asia/Shanghai",
		RequestTimeout:               2 * time.Second,
		LedgerWorkers:                1,
		ControlWorkers:               1,
		ChainWorkers:                 1,
		RateWorkers:                  1,
		BroadcastWorkers:             1,
		QueryWorkers:                 1,
		NotifyWorkers:                1,
		QueueSize:                    128,
		GroupCacheTTL:                time.Minute,
		BillSummaryCacheTTL:          time.Minute,
		UserTouchCacheTTL:            time.Minute,
		OperatorCacheTTL:             time.Minute,
		GlobalPermissionCacheTTL:     time.Minute,
		GlobalPermissionCacheSize:    32,
		WatchCacheTTL:                time.Minute,
		P2PCacheTTL:                  time.Minute,
		BotWatcherFailThreshold:      1,
		BotFallbackRecoverySuccesses: 2,
	}
	tg := telegram.NewClient(server.URL, "test", 2*time.Second)
	b := New(cfg, store, tg, nil, p2p.NewClient("", "", time.Second))
	now := time.Now().In(b.loc).Truncate(time.Microsecond)
	suffix := now.UnixNano()
	chatID := int64(-980000000000 - suffix%1000000)
	operatorID := int64(780000000000 + suffix%1000000)
	otherID := operatorID + 1
	dayKey := now.Format("2006-01-02")
	if err := store.EnsureGroup(ctx, chatID, "clear callback integration", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupCutoffState(ctx, chatID, 0, true, dayKey, dayKey, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	group, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddOperator(ctx, chatID, storage.User{ID: operatorID, DisplayName: "operator"}, operatorID, now); err != nil {
		t.Fatal(err)
	}
	recordID, err := store.InsertRecord(ctx, storage.Record{
		ChatID: chatID, DayKey: dayKey, Kind: "deposit", Currency: "CNY", Amount: "1",
		Rate: "1", FeeRate: "0", ResultUSDT: "1", ActorUserID: operatorID,
		CreatedAt: group.ActivePeriodStartedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	createTicket := func(label string, requester int64, expires time.Time) string {
		t.Helper()
		token := fmt.Sprintf("%s-%d", label, suffix)
		if err := store.CreateLedgerClearTicket(ctx, storage.LedgerClearTicket{
			TokenHash: adminauth.HashToken(token), ChatID: chatID, RequestedByUserID: requester,
			DayKey: group.ActiveDayKey, ActivePeriodStartedAt: group.ActivePeriodStartedAt,
			ExpiresAt: expires, CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		return token
	}
	callback := func(token string, userID int64) string {
		t.Helper()
		err := b.handleClearLedgerCallback(contextWithPermissionMemo(ctx), telegram.CallbackQuery{
			ID: "callback", From: telegram.User{ID: userID}, Data: ledgerClearCallbackData(token),
			Message: &telegram.Message{MessageID: 10, Chat: telegram.Chat{ID: chatID, Type: "supergroup"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		select {
		case answer := <-answers:
			if !strings.HasPrefix(answer, "answerCallbackQuery:") || answer == "answerCallbackQuery:" {
				t.Fatalf("unexpected callback response %q", answer)
			}
			return answer
		case <-time.After(time.Second):
			t.Fatal("callback answer was not sent")
			return ""
		}
	}

	revokedToken := createTicket("revoked", operatorID, now.Add(time.Minute))
	if removed, err := store.RemoveOperator(ctx, chatID, operatorID); err != nil || !removed {
		t.Fatalf("remove operator = %v, %v", removed, err)
	}
	callback(revokedToken, operatorID)
	if record, ok, err := store.GetRecord(ctx, recordID); err != nil || !ok || record.DeletedAt != nil {
		t.Fatalf("revoked callback changed record = %+v, ok=%t err=%v", record, ok, err)
	}

	otherToken := createTicket("other", operatorID, now.Add(time.Minute))
	callback(otherToken, otherID)

	if err := store.AddOperator(ctx, chatID, storage.User{ID: operatorID, DisplayName: "operator"}, operatorID, now); err != nil {
		t.Fatal(err)
	}
	expiredToken := createTicket("expired", operatorID, now.Add(-time.Second))
	callback(expiredToken, operatorID)

	periodToken := createTicket("cutoff", operatorID, now.Add(time.Minute))
	if err := store.SetGroupCutoffState(ctx, chatID, 0, true, dayKey, now.AddDate(0, 0, -1).Format("2006-01-02"), now); err != nil {
		t.Fatal(err)
	}
	callback(periodToken, operatorID)
	if record, ok, err := store.GetRecord(ctx, recordID); err != nil || !ok || record.DeletedAt != nil {
		t.Fatalf("cutoff callback changed record = %+v, ok=%t err=%v", record, ok, err)
	}
}
