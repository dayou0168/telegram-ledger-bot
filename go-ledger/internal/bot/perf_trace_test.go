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
	done := measurePerfStage(ctx, "db_record_write")
	done()

	finishPerfTrace(trace, time.Nanosecond)
	out := buf.String()
	for _, want := range []string{`"kind":"slow_update"`, `"chat_id":-100123`, `"command":"ledger_record"`, `"group":"hit"`} {
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
