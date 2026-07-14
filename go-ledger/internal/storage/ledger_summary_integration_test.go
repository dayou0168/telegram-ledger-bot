package storage

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func TestPostgresLedgerPeriodSummaryFallbackAndClear(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7000000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-14"
	nextDayKey := "2026-07-15"
	period1 := now.Add(-2 * time.Hour)
	period2 := now.Add(-time.Hour)
	if err := store.EnsureGroup(ctx, chatID, "summary", period1); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, period1); err != nil {
		t.Fatal(err)
	}

	insert := func(source int64, recordDayKey string, period time.Time, amount, result string) (int64, int64) {
		recordID, outboxID, insertErr := store.InsertLedgerRecordWithOutbox(ctx, Record{
			ChatID: chatID, DayKey: recordDayKey, PeriodStartedAt: period, Kind: "deposit", Currency: "CNY",
			Amount: amount, Rate: "10", FeeRate: "0", ResultUSDT: result,
			ActorUserID: 1, SubjectUserID: 1, SourceMessageID: source, CreatedAt: period.Add(time.Duration(source) * time.Millisecond),
		}, NotificationOutbox{Kind: "ledger_bill", DedupeKey: fmt.Sprintf("summary:%d:%d", chatID, source), ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0}, now, true)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		return recordID, outboxID
	}

	firstRecordID, firstOutbox := insert(1, dayKey, period1, "100", "10")
	replayedRecordID, replayedOutboxID := insert(1, dayKey, period1, "100", "10")
	if replayedRecordID != firstRecordID || replayedOutboxID != firstOutbox {
		t.Fatalf("dedupe replay = record/outbox %d/%d, want %d/%d", replayedRecordID, replayedOutboxID, firstRecordID, firstOutbox)
	}
	if item, ok, err := store.ClaimNotificationByID(ctx, firstOutbox, 8, now); err != nil || !ok || item.ReferenceID == 0 {
		t.Fatalf("transactional outbox claim = %+v, %v, %v", item, ok, err)
	} else if err := store.MarkNotificationSent(ctx, item.ID, 101, now); err != nil {
		t.Fatal(err)
	}
	key1 := LedgerPeriodSummaryKey{ChatID: chatID, DayKey: dayKey, PeriodStartedAt: period1}
	data, err := store.GetBillSummaryForPeriod(ctx, key1, 5, true, false, now)
	if err != nil || data.Source != "legacy" || data.Summary.DepositCount != 1 {
		t.Fatalf("first fallback = %+v, %v", data, err)
	}
	data, err = store.GetBillSummaryForPeriod(ctx, key1, 5, true, false, now)
	if err != nil || data.Source != "incremental" || !numericEqual(data.Summary.TotalDepositCNY, "100") {
		t.Fatalf("healthy summary = %+v, %v", data, err)
	}
	negativeID, _ := insert(2, dayKey, period1, "-20", "-2")
	data, err = store.GetBillSummaryForPeriod(ctx, key1, 5, true, false, now)
	if err != nil || data.Source != "incremental" || data.Summary.DepositCount != 2 || !numericEqual(data.Summary.TotalDepositCNY, "80") {
		t.Fatalf("negative delta = %+v, %v", data, err)
	}

	legacyID, err := store.InsertRecord(ctx, Record{ChatID: chatID, DayKey: dayKey,
		Kind: "deposit", Currency: "CNY", Amount: "5", Rate: "1", FeeRate: "0", ResultUSDT: "5",
		ActorUserID: 1, SubjectUserID: 1, SourceMessageID: 3, CreatedAt: period1.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := store.GetBillSummaryForPeriod(ctx, key1, 5, true, false, now)
	if err != nil || !numericEqual(stale.Summary.TotalDepositCNY, "80") {
		t.Fatalf("expected pre-compare stale shadow = %+v, %v", stale, err)
	}
	fallback, err := store.GetBillSummaryForPeriod(ctx, key1, 5, true, true, now)
	if err != nil || fallback.Source != "legacy" || !numericEqual(fallback.Summary.TotalDepositCNY, "85") {
		t.Fatalf("compare fallback = %+v, %v", fallback, err)
	}
	repaired, err := store.GetBillSummaryForPeriod(ctx, key1, 5, true, false, now)
	if err != nil || repaired.Source != "incremental" || !numericEqual(repaired.Summary.TotalDepositCNY, "85") {
		t.Fatalf("repaired summary = %+v, %v", repaired, err)
	}
	legacyRecord, legacyOutboxID, deleted, err := store.SoftDeleteRecordWithSummaryAndOutbox(ctx, chatID, legacyID, now, "deposit", period1, true,
		NotificationOutbox{Kind: "ledger_bill", DedupeKey: fmt.Sprintf("summary-legacy-undo:%d", legacyID), ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0})
	if err != nil || !deleted || legacyOutboxID == 0 || !legacyRecord.PeriodStartedAt.Equal(period1) {
		t.Fatalf("legacy-period undo upgrade = %+v, %d, %v, %v", legacyRecord, legacyOutboxID, deleted, err)
	}
	_, undoOutboxID, deleted, err := store.SoftDeleteRecordWithSummaryAndOutbox(ctx, chatID, negativeID, now, "deposit", period1, true,
		NotificationOutbox{Kind: "ledger_bill", DedupeKey: fmt.Sprintf("summary-undo:%d", negativeID), ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0})
	if err != nil || !deleted || undoOutboxID == 0 {
		t.Fatalf("transactional undo summary/outbox = %v, %d, %v", deleted, undoOutboxID, err)
	}
	data, _ = store.GetBillSummaryForPeriod(ctx, key1, 5, true, false, now)
	if data.Summary.DepositCount != 1 || !numericEqual(data.Summary.TotalDepositCNY, "100") {
		t.Fatalf("undo delta = %+v", data.Summary)
	}

	if err := store.SetGroupActive(ctx, chatID, false, "", period2.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, nextDayKey, nextDayKey, period2); err != nil {
		t.Fatal(err)
	}
	insert(4, nextDayKey, period2, "50", "5")
	insert(5, nextDayKey, period2, "25", "2.5")
	key2 := LedgerPeriodSummaryKey{ChatID: chatID, DayKey: nextDayKey, PeriodStartedAt: period2}
	if data, err = store.GetBillSummaryForPeriod(ctx, key2, 5, true, false, now); err != nil || data.Summary.DepositCount != 2 {
		t.Fatalf("next-cutoff period = %+v, %v", data, err)
	}
	period1BeforeClear, err := store.GetBillSummaryForPeriod(ctx, key1, 5, true, true, now)
	if err != nil || period1BeforeClear.Summary.DepositCount != 1 || !numericEqual(period1BeforeClear.Summary.TotalDepositCNY, "100") {
		t.Fatalf("next-cutoff period leaked into prior period: %+v, %v", period1BeforeClear, err)
	}
	group, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	ticket := LedgerClearTicket{TokenHash: fmt.Sprintf("clear-%d", now.UnixNano()), ChatID: chatID,
		RequestedByUserID: 1, DayKey: nextDayKey, ActivePeriodStartedAt: period2,
		ExpiresAt: now.Add(time.Minute), CreatedAt: now}
	if err := store.CreateLedgerClearTicket(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	result, err := store.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, 1, true, group, now.Add(time.Second))
	if err != nil || result.Status != LedgerClearTicketApplied || result.DeletedCount != 2 {
		t.Fatalf("clear current period = %+v, %v", result, err)
	}
	cleared, err := store.GetBillSummaryForPeriod(ctx, key2, 5, true, false, now.Add(time.Second))
	if err != nil || cleared.Summary.DepositCount != 0 || !numericEqual(cleared.Summary.TotalDepositCNY, "0") {
		t.Fatalf("cleared summary = %+v, %v", cleared, err)
	}
	period1Data, _ := store.GetBillSummaryForPeriod(ctx, key1, 5, true, true, now)
	if period1Data.Summary.DepositCount != 1 {
		t.Fatalf("clear touched sealed period: %+v", period1Data.Summary)
	}

	stats1, err := store.ReconcileLedgerSummaries(ctx, 0, 1000, now)
	if err != nil || stats1.Scanned == 0 {
		t.Fatalf("first reconcile = %+v, %v", stats1, err)
	}
	stats2, err := store.ReconcileLedgerSummaries(ctx, 0, 1000, now.Add(time.Second))
	if err != nil || stats2.Scanned != stats1.Scanned {
		t.Fatalf("reentrant reconcile = %+v, %v", stats2, err)
	}
}

func TestPostgresLedgerRecordOutboxDedupeConcurrent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7020000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-14"
	periodStart := now.Add(-time.Minute)
	if err := store.EnsureGroup(ctx, chatID, "dedupe", periodStart); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, periodStart); err != nil {
		t.Fatal(err)
	}

	type result struct {
		recordID int64
		outboxID int64
		err      error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recordID, outboxID, insertErr := store.InsertLedgerRecordWithOutbox(ctx, Record{
				ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
				Kind: "deposit", Currency: "CNY", Amount: "100", Rate: "10", FeeRate: "0",
				ResultUSDT: "10", ActorUserID: 1, SubjectUserID: 1,
				SourceMessageID: 77, CreatedAt: now,
			}, NotificationOutbox{
				Kind: "ledger_bill", DedupeKey: fmt.Sprintf("concurrent-dedupe:%d", chatID),
				ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0,
			}, now, true)
			results <- result{recordID: recordID, outboxID: outboxID, err: insertErr}
		}()
	}
	wg.Wait()
	close(results)
	var first result
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first.recordID == 0 {
			first = got
		} else if got.recordID != first.recordID || got.outboxID != first.outboxID {
			t.Fatalf("dedupe IDs differ: first=%+v got=%+v", first, got)
		}
	}
	var recordCount int64
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM records WHERE chat_id=$1 AND source_message_id=77`, chatID).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != 1 {
		t.Fatalf("dedupe record count = %d", recordCount)
	}
	data, err := store.GetBillSummaryForPeriod(ctx, LedgerPeriodSummaryKey{ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart}, 5, true, false, now)
	if err != nil || data.Summary.DepositCount != 1 || !numericEqual(data.Summary.TotalDepositCNY, "100") {
		t.Fatalf("dedupe summary = %+v, %v", data, err)
	}
}

func TestPostgresLedgerPeriodSummaryCurrenciesUndoAndRollback(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7040000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-14"
	periodStart := now.Add(-time.Hour)
	if err := store.EnsureGroup(ctx, chatID, "currencies", periodStart); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, periodStart); err != nil {
		t.Fatal(err)
	}
	insert := func(source int64, kind, currency, amount, result string) int64 {
		recordID, _, insertErr := store.InsertLedgerRecordWithOutbox(ctx, Record{
			ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
			Kind: kind, Currency: currency, Amount: amount, Rate: "7", FeeRate: "3", ResultUSDT: result,
			ActorUserID: 1, SubjectUserID: 1, SourceMessageID: source,
			CreatedAt: periodStart.Add(time.Duration(source) * time.Minute),
		}, NotificationOutbox{
			Kind: "ledger_bill", DedupeKey: fmt.Sprintf("currencies:%d:%d", chatID, source),
			ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0,
		}, now, true)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		return recordID
	}
	insert(1, "deposit", "CNY", "100", "10")
	usdtDepositID := insert(2, "deposit", "USDT", "5", "5")
	insert(3, "payout", "USDT", "4", "4")
	negativePayoutID := insert(4, "payout", "USDT", "-1", "-1")
	key := LedgerPeriodSummaryKey{ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart}
	data, err := store.GetBillSummaryForPeriod(ctx, key, 5, true, false, now)
	if err != nil || data.Summary.DepositCount != 2 || data.Summary.PayoutCount != 2 ||
		!numericEqual(data.Summary.TotalDepositCNY, "100") || !numericEqual(data.Summary.TotalDepositUSDT, "15") ||
		!numericEqual(data.Summary.TotalPayoutUSDT, "3") {
		t.Fatalf("currency summary = %+v, %v", data.Summary, err)
	}
	for _, target := range []struct {
		id   int64
		kind string
		key  string
	}{{usdtDepositID, "deposit", "undo-usdt"}, {negativePayoutID, "payout", "undo-negative-payout"}} {
		if _, _, deleted, deleteErr := store.SoftDeleteRecordWithSummaryAndOutbox(ctx, chatID, target.id, now, target.kind, periodStart, true,
			NotificationOutbox{Kind: "ledger_bill", DedupeKey: fmt.Sprintf("%s:%d", target.key, chatID), ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0}); deleteErr != nil || !deleted {
			t.Fatalf("undo %s = %v, %v", target.kind, deleted, deleteErr)
		}
	}
	data, err = store.GetBillSummaryForPeriod(ctx, key, 5, true, false, now)
	if err != nil || data.Summary.DepositCount != 1 || data.Summary.PayoutCount != 1 ||
		!numericEqual(data.Summary.TotalDepositCNY, "100") || !numericEqual(data.Summary.TotalDepositUSDT, "10") ||
		!numericEqual(data.Summary.TotalPayoutUSDT, "4") {
		t.Fatalf("summary after mixed undo = %+v, %v", data.Summary, err)
	}

	badDedupe := fmt.Sprintf("rollback-bad-numeric:%d", chatID)
	if _, _, err := store.InsertLedgerRecordWithOutbox(ctx, Record{
		ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
		Kind: "deposit", Currency: "CNY", Amount: "not-numeric", Rate: "1", FeeRate: "0", ResultUSDT: "1",
		ActorUserID: 1, SubjectUserID: 1, SourceMessageID: 99, CreatedAt: now,
	}, NotificationOutbox{Kind: "ledger_bill", DedupeKey: badDedupe, ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0}, now, true); err == nil {
		t.Fatal("invalid numeric summary mutation should fail")
	}
	var recordCount, outboxCount int64
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM records WHERE chat_id=$1 AND source_message_id=99`, chatID).Scan(&recordCount); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notification_outbox WHERE dedupe_key=$1`, badDedupe).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if recordCount != 0 || outboxCount != 0 {
		t.Fatalf("failed transaction leaked record/outbox = %d/%d", recordCount, outboxCount)
	}
}

func TestPostgresLedgerSummaryBackfillUsesRecordKeysetAndPeriodReconcile(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7050000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-14"
	if _, err := store.pool.Exec(ctx, `INSERT INTO records(
		chat_id,day_key,kind,currency,amount,rate,fee_rate,result_usdt,
		subject_user_id,subject_name,actor_user_id,actor_name,source_message_id,
		bot_message_id,remark,created_at
	) SELECT $1,$2,'deposit','CNY','1','1','0','1',1,'legacy',1,'legacy',n,0,'',$3::timestamptz+n*interval '1 millisecond'
	FROM generate_series(1,2500) n`, chatID, dayKey, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE ledger_summary_backfill_state
		SET last_record_id=0,completed_at=NULL,updated_at=$1 WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	stats, err := store.ReconcileLedgerSummaries(ctx, 0, 1000, now)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Scanned == 0 || stats.Scanned >= 2500 {
		t.Fatalf("reconcile scanned records instead of period keys: %+v", stats)
	}
	var depositCount int64
	var dirty bool
	if err := store.pool.QueryRow(ctx, `SELECT deposit_count,dirty
		FROM ledger_period_summaries WHERE chat_id=$1 AND day_key=$2 AND period_started_at=$3`,
		chatID, dayKey, ledgerPeriodEpoch).Scan(&depositCount, &dirty); err != nil {
		t.Fatal(err)
	}
	if dirty || depositCount != 2500 {
		t.Fatalf("backfilled summary = count %d dirty %v", depositCount, dirty)
	}
	var completed bool
	var lastRecordID, chatMaxRecordID int64
	if err := store.pool.QueryRow(ctx, `SELECT last_record_id,completed_at IS NOT NULL
		FROM ledger_summary_backfill_state WHERE id=1`).Scan(&lastRecordID, &completed); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT MAX(id) FROM records WHERE chat_id=$1`, chatID).Scan(&chatMaxRecordID); err != nil {
		t.Fatal(err)
	}
	if !completed || lastRecordID < chatMaxRecordID {
		t.Fatalf("backfill progress = last %d chat max %d completed %v", lastRecordID, chatMaxRecordID, completed)
	}
	lateRecordID, err := store.InsertRecord(ctx, Record{
		ChatID: chatID, DayKey: dayKey, Kind: "deposit", Currency: "CNY", Amount: "1", Rate: "1", FeeRate: "0", ResultUSDT: "1",
		ActorUserID: 1, SubjectUserID: 1, SourceMessageID: 3000, CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReconcileLedgerSummaries(ctx, lastRecordID, 1000, now.Add(time.Second)); err != nil {
		t.Fatalf("reentrant reconcile: %v", err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT last_record_id FROM ledger_summary_backfill_state WHERE id=1`).Scan(&lastRecordID); err != nil {
		t.Fatal(err)
	}
	if lastRecordID < lateRecordID {
		t.Fatalf("late legacy record was not discovered: last %d record %d", lastRecordID, lateRecordID)
	}
	if err := store.pool.QueryRow(ctx, `SELECT deposit_count,dirty FROM ledger_period_summaries
		WHERE chat_id=$1 AND day_key=$2 AND period_started_at=$3`, chatID, dayKey, ledgerPeriodEpoch).Scan(&depositCount, &dirty); err != nil {
		t.Fatal(err)
	}
	if dirty || depositCount != 2501 {
		t.Fatalf("late legacy record summary = count %d dirty %v", depositCount, dirty)
	}
}

func TestPostgresLedgerSummaryReconcileSerializesConcurrentWrite(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7060000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-14"
	periodStart := now.Add(-time.Hour)
	key := LedgerPeriodSummaryKey{ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart}
	if err := store.EnsureGroup(ctx, chatID, "reconcile race", periodStart); err != nil {
		t.Fatal(err)
	}
	insert := func(source int64) error {
		_, _, insertErr := store.InsertLedgerRecordWithOutbox(ctx, Record{
			ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
			Kind: "deposit", Currency: "CNY", Amount: "1", Rate: "1", FeeRate: "0", ResultUSDT: "1",
			ActorUserID: 1, SubjectUserID: 1, SourceMessageID: source, CreatedAt: now.Add(time.Duration(source) * time.Microsecond),
		}, NotificationOutbox{
			Kind: "ledger_bill", DedupeKey: fmt.Sprintf("reconcile-race:%d:%d", chatID, source),
			ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0,
		}, now, true)
		return insertErr
	}
	if err := insert(1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetBillSummaryForPeriod(ctx, key, 5, true, false, now); err != nil {
		t.Fatal(err)
	}

	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(ctx, tx)
	if err := lockLedgerPeriod(ctx, tx, key); err != nil {
		t.Fatal(err)
	}
	authoritative, err := getLegacyBillSummaryForPeriodUsing(ctx, tx, key, 1)
	if err != nil || authoritative.Summary.DepositCount != 1 {
		t.Fatalf("locked authoritative summary = %+v, %v", authoritative, err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- insert(2) }()
	select {
	case err := <-writeDone:
		t.Fatalf("concurrent write bypassed period lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := replaceLedgerPeriodSummaryUsing(ctx, tx, key, authoritative.Summary, now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent write did not resume after reconcile commit")
	}
	data, err := store.GetBillSummaryForPeriod(ctx, key, 5, true, false, now.Add(time.Second))
	if err != nil || data.Summary.DepositCount != 2 || !numericEqual(data.Summary.TotalDepositCNY, "2") {
		t.Fatalf("concurrent reconcile lost write = %+v, %v", data.Summary, err)
	}
}

func TestPostgresSameBusinessDayPauseResumeSharesLedgerPeriod(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7010000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-14"
	periodStart := now.Add(-time.Hour)
	if err := store.EnsureGroup(ctx, chatID, "pause resume", periodStart); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, periodStart); err != nil {
		t.Fatal(err)
	}
	insert := func(source int64, amount string) int64 {
		recordID, _, insertErr := store.InsertLedgerRecordWithOutbox(ctx, Record{
			ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
			Kind: "deposit", Currency: "CNY", Amount: amount, Rate: "10", FeeRate: "0",
			ResultUSDT: amount, ActorUserID: 1, SubjectUserID: 1,
			SourceMessageID: source, CreatedAt: periodStart.Add(time.Duration(source) * time.Minute),
		}, NotificationOutbox{
			Kind: "ledger_bill", DedupeKey: fmt.Sprintf("pause-resume:%d:%d", chatID, source),
			ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0,
		}, now, true)
		if insertErr != nil {
			t.Fatal(insertErr)
		}
		return recordID
	}
	firstID := insert(1, "100")
	if err := store.SetGroupActivePeriod(ctx, chatID, false, "", "", now); err != nil {
		t.Fatal(err)
	}
	paused, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Active || paused.ActiveDayKey != dayKey || !paused.ActivePeriodStartedAt.Equal(periodStart) {
		t.Fatalf("paused period was not retained: %+v", paused)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Active || !resumed.ActivePeriodStartedAt.Equal(periodStart) {
		t.Fatalf("same-day resume changed period start: %+v", resumed)
	}
	insert(2, "50")
	key := LedgerPeriodSummaryKey{ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart}
	data, err := store.GetBillSummaryForPeriod(ctx, key, 5, true, false, now.Add(2*time.Minute))
	if err != nil || data.Summary.DepositCount != 2 || !numericEqual(data.Summary.TotalDepositCNY, "150") || len(data.Records) != 2 {
		t.Fatalf("resumed period summary/recent = %+v, %v", data, err)
	}
	_, _, deleted, err := store.SoftDeleteRecordWithSummaryAndOutbox(ctx, chatID, firstID, now.Add(3*time.Minute), "deposit", periodStart, true,
		NotificationOutbox{Kind: "ledger_bill", DedupeKey: fmt.Sprintf("pause-resume-undo:%d", firstID), ChatID: chatID, ReferenceKind: "ledger_record", Priority: 0})
	if err != nil || !deleted {
		t.Fatalf("undo pre-pause record = %v, %v", deleted, err)
	}
	data, err = store.GetBillSummaryForPeriod(ctx, key, 5, true, false, now.Add(4*time.Minute))
	if err != nil || data.Summary.DepositCount != 1 || !numericEqual(data.Summary.TotalDepositCNY, "50") {
		t.Fatalf("summary after pre-pause undo = %+v, %v", data, err)
	}
}

func TestPostgresPausedLedgerPeriodPersistsAcrossStoreReopen(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := -7030000000000 - now.UnixNano()%1000000
	periodStart := now.Add(-time.Hour)
	if err := store.EnsureGroup(ctx, chatID, "reopen", periodStart); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SetGroupCutoffState(ctx, chatID, -1, true, "2026-07-01", "", periodStart); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, false, "", "", now); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	reopened, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	group, err := reopened.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatal(err)
	}
	if group.Active || group.CutoffHour != -1 || group.ActiveDayKey != "2026-07-01" || !group.ActivePeriodStartedAt.Equal(periodStart) {
		t.Fatalf("paused period was not restored after reopen: %+v", group)
	}
}

func TestPostgresCriticalOutboxFastpathOrderingBulkAndCrashRecovery(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	chatID := -7100000000000 - now.UnixNano()%1000000
	prefix := fmt.Sprintf("bulk-fastpath-%d", now.UnixNano())
	if _, err := store.pool.Exec(ctx, `INSERT INTO notification_outbox(
		kind,dedupe_key,chat_id,text,priority,status,attempts,next_attempt_at,created_at,updated_at)
	SELECT 'bulk',$1||':'||n,$2,'bulk',2,'pending',0,$3,$3,$3 FROM generate_series(1,10000) n`, prefix, chatID, now); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = store.pool.Exec(context.Background(), `DELETE FROM notification_outbox WHERE dedupe_key LIKE $1`, prefix+"%")
	}()
	for i := 1; i <= 2; i++ {
		if _, err := store.pool.Exec(ctx, `INSERT INTO notification_outbox(
			kind,dedupe_key,chat_id,text,priority,status,attempts,next_attempt_at,created_at,updated_at)
		VALUES('ledger_bill',$1,$2,'critical',0,'pending',0,$3,$3,$3)`, fmt.Sprintf("%s:c:%d", prefix, i), chatID, now); err != nil {
			t.Fatal(err)
		}
	}
	var firstID, secondID int64
	if err := store.pool.QueryRow(ctx, `SELECT MIN(id),MAX(id) FROM notification_outbox WHERE dedupe_key LIKE $1`, prefix+":c:%").Scan(&firstID, &secondID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, ok, err := store.ClaimNotificationByID(ctx, secondID, 8, now); err != nil || ok {
		t.Fatalf("second critical bypassed first: ok=%v err=%v", ok, err)
	}
	first, ok, err := store.ClaimNotificationByID(ctx, firstID, 8, now)
	if err != nil || !ok || time.Since(started) > 5*time.Second {
		t.Fatalf("critical blocked by 10k bulk: ok=%v err=%v elapsed=%v", ok, err, time.Since(started))
	}
	if _, ok, err := store.ClaimNotificationByID(ctx, firstID, 8, now.Add(time.Second)); err != nil || ok {
		t.Fatalf("non-stale sending row reclaimed: ok=%v err=%v", ok, err)
	}
	recovered, ok, err := store.ClaimNotificationByID(ctx, firstID, 8, now.Add(3*time.Minute))
	if err != nil || !ok || recovered.Attempts != 2 {
		t.Fatalf("claim-after-crash recovery = %+v ok=%v err=%v", recovered, ok, err)
	}
	if err := store.MarkNotificationSent(ctx, first.ID, 100, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	second, ok, err := store.ClaimNotificationByID(ctx, secondID, 8, now.Add(3*time.Minute))
	if err != nil || !ok {
		t.Fatalf("second critical not released after first sent: ok=%v err=%v", ok, err)
	}
	if err := store.MarkNotificationSent(ctx, second.ID, 101, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	lostWakeKey := prefix + ":c:lost-wake"
	if inserted, err := store.EnqueueNotification(ctx, NotificationOutbox{Kind: "ledger_bill", DedupeKey: lostWakeKey, ChatID: chatID, Priority: 0}, now.Add(4*time.Minute)); err != nil || !inserted {
		t.Fatalf("enqueue lost-wake row = %v, %v", inserted, err)
	}
	items, err := store.ClaimDueNotifications(ctx, 50, 8, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.DedupeKey == lostWakeKey {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("generic scheduler did not recover a committed critical row after a lost wake")
	}
}
