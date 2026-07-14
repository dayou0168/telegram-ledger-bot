package bot

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestInvalidateGroupCacheRemovesCachedSettings(t *testing.T) {
	b := &Bot{groupCache: newTTLCache[storage.Group](time.Minute)}
	b.groupCache.Set("123", storage.Group{ChatID: 123, FeeRate: "3"})
	b.invalidateGroupCache(123)
	if _, ok := b.groupCache.Get("123"); ok {
		t.Fatal("group cache should be invalidated immediately")
	}
}

func TestInvalidateBillSummaryCacheRemovesCachedDay(t *testing.T) {
	b := &Bot{billSummaryCache: newTTLCache[storage.BillSummaryData](time.Minute)}
	periodStart := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	key := billSummaryCacheKey(123, "2026-07-11", periodStart, groupBillDefaultRecordLimit)
	b.billSummaryCache.Set(key, storage.BillSummaryData{
		Summary: storage.RecordDaySummary{DepositCount: 1, TotalDepositUSDT: "100"},
	})
	b.invalidateBillSummaryCache(123, "2026-07-11")
	if _, ok := b.billSummaryCache.Get(key); ok {
		t.Fatal("bill summary cache should be invalidated after ledger writes")
	}
}

func TestInvalidateLedgerPermissionRemovesOperator(t *testing.T) {
	b := &Bot{operatorCache: newTTLCache[bool](time.Minute)}
	b.operatorCache.Set(ledgerPermissionCacheKey(-1001, 2002), true)

	b.InvalidateLedgerPermission(-1001, 2002)

	if _, ok := b.operatorCache.Get(ledgerPermissionCacheKey(-1001, 2002)); ok {
		t.Fatal("ledger operator cache should be invalidated immediately")
	}
}

func TestInvalidateBroadcastPermissionRemovesState(t *testing.T) {
	b := &Bot{
		privateStates: newTTLCache[privateState](time.Minute),
	}
	b.privateStates.Set(formatID(2002), privateState{Mode: "all", ChatIDs: []int64{-1001}})

	b.InvalidateBroadcastPermission(2002)

	if _, ok := b.privateStates.Get(formatID(2002)); ok {
		t.Fatal("broadcast private state should be invalidated immediately")
	}
}

func TestInvalidateAllPermissionCachesClearsCapabilityInputs(t *testing.T) {
	b := &Bot{globalCapabilityCache: newGlobalCapabilityCache(time.Minute, 4)}
	b.globalCapabilityCache.Set(2002, 1, globalCapabilityValue{Active: true})
	b.InvalidateAllPermissionCaches()
	if b.globalCapabilityCache.Len() != 0 {
		t.Fatal("global capability input cache should be invalidated immediately")
	}
}

func TestInvalidateWatchTargetsClearsCachedTargets(t *testing.T) {
	b := &Bot{watchTargetCache: newTTLCache[[]storage.WatchTarget](time.Minute)}
	b.watchTargetCache.Set("all", []storage.WatchTarget{{OwnerUserID: 2002, Address: "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ"}})

	b.InvalidateWatchTargets()

	if _, ok := b.watchTargetCache.Get("all"); ok {
		t.Fatal("watch target cache should be invalidated immediately")
	}
}

func TestIntersectChatIDsFiltersStaleBroadcastTargets(t *testing.T) {
	got := intersectChatIDs([]int64{-1001, -1002, -1003}, []int64{-1002, -1004})
	if len(got) != 1 || got[0] != -1002 {
		t.Fatalf("intersectChatIDs = %v, want [-1002]", got)
	}
}

func TestPerfTraceSlowLogRedactsMessageText(t *testing.T) {
	var buf bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer log.SetOutput(oldWriter)
	defer log.SetFlags(oldFlags)

	trace := newPerfTrace(777, -100123)
	trace.startedAt = time.Now().Add(-time.Second)
	ctx := contextWithPerfTrace(context.Background(), trace)
	setPerfCommand(ctx, "ledger_record")
	markPerfCache(ctx, "group", true)
	markPerfQueue(ctx, "ledger:-100123", 7)
	addPerfStage(ctx, "queue_wait", 25*time.Millisecond)
	done := measurePerfStage(ctx, "db_record_write")
	done()

	finishPerfTrace(trace, time.Nanosecond)
	out := buf.String()
	for _, want := range []string{`"kind":"slow_update"`, `"chat_id":-100123`, `"command":"ledger_record"`, `"group":"hit"`, `"queue_key":"ledger:-100123"`, `"queue_depth":7`, `"queue_wait":25`} {
		if !strings.Contains(out, want) {
			t.Fatalf("slow log missing %q: %s", want, out)
		}
	}
	for _, leaked := range []string{"+100", "下发100U", "TELEGRAM_BOT_TOKEN", "postgres://"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("slow log leaked sensitive text %q: %s", leaked, out)
		}
	}
}
