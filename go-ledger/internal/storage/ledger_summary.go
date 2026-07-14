package storage

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ledgerPeriodEpoch = time.Unix(0, 0).UTC()

const ledgerSummaryBackfillBatchSize = 5000

type ledgerSummaryQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type ledgerSummaryDB interface {
	ledgerSummaryQueryer
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func lockLedgerPeriod(ctx context.Context, tx pgx.Tx, key LedgerPeriodSummaryKey) error {
	lockKey := fmt.Sprintf("ledger-summary:%d:%s:%d", key.ChatID, key.DayKey, key.PeriodStartedAt.UnixMicro())
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey)
	return err
}

// InsertLedgerRecordWithOutbox commits the ledger mutation, shadow summary
// delta, and durable receipt as one unit. The records table remains canonical.
func (s *Store) InsertLedgerRecordWithOutbox(ctx context.Context, r Record, item NotificationOutbox, now time.Time, writeSummary bool) (int64, int64, error) {
	if r.PeriodStartedAt.IsZero() {
		return 0, 0, errors.New("ledger period start is required")
	}
	item.Kind = strings.TrimSpace(item.Kind)
	item.DedupeKey = strings.TrimSpace(item.DedupeKey)
	if item.Kind == "" || item.DedupeKey == "" {
		return 0, 0, errors.New("notification kind and dedupe key are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer rollback(ctx, tx)
	if writeSummary {
		if err := lockLedgerPeriod(ctx, tx, LedgerPeriodSummaryKey{ChatID: r.ChatID, DayKey: r.DayKey, PeriodStartedAt: r.PeriodStartedAt}); err != nil {
			return 0, 0, err
		}
	}

	// Reserve the durable dedupe key before mutating the ledger. This makes a
	// concurrent/replayed update return the original record instead of adding a
	// second record and merely colliding when its receipt is inserted.
	var outboxID int64
	err = tx.QueryRow(ctx, `INSERT INTO notification_outbox(
		kind, dedupe_key, chat_id, text, parse_mode, disable_preview,
		reply_to_message_id, reply_markup_json, reference_kind, reference_id,
		priority, status, attempts, next_attempt_at, created_at, updated_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,'pending',0,$11,$11,$11)
	ON CONFLICT(dedupe_key) DO NOTHING
	RETURNING id`, item.Kind, item.DedupeKey, item.ChatID, item.Text,
		item.ParseMode, item.DisablePreview, item.ReplyToMessageID,
		item.ReplyMarkupJSON, item.ReferenceKind, item.Priority, now).Scan(&outboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingRecordID, existingChatID int64
		var existingKind, existingReferenceKind string
		if err := tx.QueryRow(ctx, `SELECT id,chat_id,kind,reference_kind,reference_id
			FROM notification_outbox WHERE dedupe_key=$1`, item.DedupeKey).Scan(
			&outboxID, &existingChatID, &existingKind, &existingReferenceKind, &existingRecordID); err != nil {
			return 0, 0, err
		}
		if existingRecordID <= 0 || existingChatID != item.ChatID || existingKind != item.Kind || existingReferenceKind != item.ReferenceKind {
			return 0, 0, errors.New("notification dedupe key belongs to a different ledger receipt")
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, 0, err
		}
		return existingRecordID, outboxID, nil
	}
	if err != nil {
		return 0, 0, err
	}

	var recordID int64
	err = tx.QueryRow(ctx, `INSERT INTO records(
		chat_id, day_key, kind, currency, amount, rate, fee_rate, result_usdt,
		subject_user_id, subject_name, actor_user_id, actor_name, source_message_id,
		bot_message_id, remark, period_started_at, created_at
	) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	RETURNING id`, r.ChatID, r.DayKey, r.Kind, r.Currency, r.Amount, r.Rate,
		r.FeeRate, r.ResultUSDT, r.SubjectUserID, r.SubjectName, r.ActorUserID,
		r.ActorName, r.SourceMessageID, r.BotMessageID, r.Remark,
		r.PeriodStartedAt, r.CreatedAt).Scan(&recordID)
	if err != nil {
		return 0, 0, err
	}
	if writeSummary {
		if err := applyLedgerSummaryInsert(ctx, tx, r, recordID, now); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE notification_outbox SET reference_id=$1 WHERE id=$2`, recordID, outboxID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	return recordID, outboxID, nil
}

func applyLedgerSummaryInsert(ctx context.Context, tx pgx.Tx, r Record, recordID int64, now time.Time) error {
	depositCount, payoutCount := int64(0), int64(0)
	depositCNY, depositUSDT, payoutUSDT := "0", "0", "0"
	switch r.Kind {
	case "deposit":
		depositCount = 1
		depositUSDT = r.ResultUSDT
		if strings.EqualFold(r.Currency, "CNY") {
			depositCNY = r.Amount
		}
	case "payout":
		payoutCount = 1
		payoutUSDT = r.ResultUSDT
	}
	_, err := tx.Exec(ctx, `INSERT INTO ledger_period_summaries(
		chat_id, day_key, period_started_at, deposit_count, payout_count,
		total_deposit_cny, total_deposit_usdt, total_payout_usdt,
		high_watermark_id, dirty, updated_at
	) VALUES($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,$9,TRUE,$10)
	ON CONFLICT(chat_id,day_key,period_started_at) DO UPDATE SET
		deposit_count=ledger_period_summaries.deposit_count+excluded.deposit_count,
		payout_count=ledger_period_summaries.payout_count+excluded.payout_count,
		total_deposit_cny=ledger_period_summaries.total_deposit_cny+excluded.total_deposit_cny,
		total_deposit_usdt=ledger_period_summaries.total_deposit_usdt+excluded.total_deposit_usdt,
		total_payout_usdt=ledger_period_summaries.total_payout_usdt+excluded.total_payout_usdt,
		high_watermark_id=GREATEST(ledger_period_summaries.high_watermark_id,excluded.high_watermark_id),
		dirty=ledger_period_summaries.dirty,
		updated_at=excluded.updated_at`, r.ChatID, r.DayKey, r.PeriodStartedAt,
		depositCount, payoutCount, depositCNY, depositUSDT, payoutUSDT, recordID, now)
	return err
}

func (s *Store) SoftDeleteRecordWithSummary(ctx context.Context, chatID, recordID int64, now time.Time, kind string, writeSummary bool) (Record, bool, error) {
	record, _, deleted, err := s.softDeleteRecordWithSummaryAndOutbox(ctx, chatID, recordID, now, kind, time.Time{}, writeSummary, nil)
	return record, deleted, err
}

func (s *Store) SoftDeleteRecordWithSummaryAndOutbox(ctx context.Context, chatID, recordID int64, now time.Time, kind string, periodStart time.Time, writeSummary bool, item NotificationOutbox) (Record, int64, bool, error) {
	return s.softDeleteRecordWithSummaryAndOutbox(ctx, chatID, recordID, now, kind, periodStart, writeSummary, &item)
}

func (s *Store) softDeleteRecordWithSummaryAndOutbox(ctx context.Context, chatID, recordID int64, now time.Time, kind string, periodStart time.Time, writeSummary bool, item *NotificationOutbox) (Record, int64, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, 0, false, err
	}
	defer rollback(ctx, tx)
	query := `UPDATE records SET deleted_at=$1 WHERE chat_id=$2 AND id=$3 AND deleted_at IS NULL`
	args := []any{now, chatID, recordID}
	if kind != "" {
		query += ` AND kind=$4`
		args = append(args, kind)
	}
	query += ` RETURNING ` + recordSelectColumns
	record, err := scanRecord(tx.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, 0, false, tx.Commit(ctx)
	}
	if err != nil {
		return Record{}, 0, false, err
	}
	if !periodStart.IsZero() && periodStart.After(ledgerPeriodEpoch) && !record.PeriodStartedAt.After(ledgerPeriodEpoch) {
		if _, err := tx.Exec(ctx, `UPDATE records SET period_started_at=$1 WHERE id=$2`, periodStart, record.ID); err != nil {
			return Record{}, 0, false, err
		}
		record.PeriodStartedAt = periodStart
	}
	if writeSummary && !record.PeriodStartedAt.IsZero() && record.PeriodStartedAt.After(ledgerPeriodEpoch) {
		if err := lockLedgerPeriod(ctx, tx, LedgerPeriodSummaryKey{ChatID: record.ChatID, DayKey: record.DayKey, PeriodStartedAt: record.PeriodStartedAt}); err != nil {
			return Record{}, 0, false, err
		}
		if err := applyLedgerSummaryDelete(ctx, tx, record, now); err != nil {
			return Record{}, 0, false, err
		}
	}
	var outboxID int64
	if item != nil {
		item.Kind = strings.TrimSpace(item.Kind)
		item.DedupeKey = strings.TrimSpace(item.DedupeKey)
		if item.Kind == "" || item.DedupeKey == "" {
			return Record{}, 0, false, errors.New("notification kind and dedupe key are required")
		}
		item.ReferenceID = record.ID
		err = tx.QueryRow(ctx, `INSERT INTO notification_outbox(
			kind,dedupe_key,chat_id,text,parse_mode,disable_preview,reply_to_message_id,
			reply_markup_json,reference_kind,reference_id,priority,status,attempts,
			next_attempt_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'pending',0,$12,$12,$12)
		ON CONFLICT(dedupe_key) DO UPDATE SET dedupe_key=excluded.dedupe_key
		RETURNING id`, item.Kind, item.DedupeKey, item.ChatID, item.Text, item.ParseMode,
			item.DisablePreview, item.ReplyToMessageID, item.ReplyMarkupJSON,
			item.ReferenceKind, item.ReferenceID, item.Priority, now).Scan(&outboxID)
		if err != nil {
			return Record{}, 0, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Record{}, 0, false, err
	}
	return record, outboxID, true, nil
}

func applyLedgerSummaryDelete(ctx context.Context, tx pgx.Tx, r Record, now time.Time) error {
	depositCount, payoutCount := int64(0), int64(0)
	depositCNY, depositUSDT, payoutUSDT := "0", "0", "0"
	if r.Kind == "deposit" {
		depositCount = 1
		depositUSDT = r.ResultUSDT
		if strings.EqualFold(r.Currency, "CNY") {
			depositCNY = r.Amount
		}
	} else if r.Kind == "payout" {
		payoutCount = 1
		payoutUSDT = r.ResultUSDT
	}
	_, err := tx.Exec(ctx, `UPDATE ledger_period_summaries SET
		deposit_count=deposit_count-$4,
		payout_count=payout_count-$5,
		total_deposit_cny=total_deposit_cny-$6::numeric,
		total_deposit_usdt=total_deposit_usdt-$7::numeric,
		total_payout_usdt=total_payout_usdt-$8::numeric,
		updated_at=$9
	WHERE chat_id=$1 AND day_key=$2 AND period_started_at=$3`, r.ChatID,
		r.DayKey, r.PeriodStartedAt, depositCount, payoutCount, depositCNY,
		depositUSDT, payoutUSDT, now)
	return err
}

func (s *Store) GetBillSummaryForPeriod(ctx context.Context, key LedgerPeriodSummaryKey, recentLimit int, useSummary, compare bool, now time.Time) (BillSummaryData, error) {
	if recentLimit < 1 {
		recentLimit = 1
	}
	if useSummary && !compare {
		data, ok, err := s.readHealthyLedgerSummaryWithRecords(ctx, key, recentLimit)
		if err != nil {
			return BillSummaryData{}, err
		}
		if ok {
			return data, nil
		}
	}
	if !useSummary {
		legacy, err := s.getLegacyBillSummaryForPeriod(ctx, key, recentLimit)
		legacy.Source = "legacy"
		return legacy, err
	}
	legacy, _, err := s.loadAndRepairLedgerPeriodSummary(ctx, key, recentLimit, now)
	if err != nil {
		return BillSummaryData{}, err
	}
	legacy.Source = "legacy"
	return legacy, nil
}

func (s *Store) readHealthyLedgerSummaryWithRecords(ctx context.Context, key LedgerPeriodSummaryKey, recentLimit int) (BillSummaryData, bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return BillSummaryData{}, false, err
	}
	defer rollback(ctx, tx)
	data, ok, err := readHealthyLedgerSummaryUsing(ctx, tx, key)
	if err != nil || !ok {
		return data, ok, err
	}
	data.Records, err = listRecentRecordsForPeriodUsing(ctx, tx, key, recentLimit)
	if err != nil {
		return BillSummaryData{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BillSummaryData{}, false, err
	}
	return data, true, nil
}

func (s *Store) readHealthyLedgerSummary(ctx context.Context, key LedgerPeriodSummaryKey) (BillSummaryData, bool, error) {
	return readHealthyLedgerSummaryUsing(ctx, s.pool, key)
}

func readHealthyLedgerSummaryUsing(ctx context.Context, db ledgerSummaryQueryer, key LedgerPeriodSummaryKey) (BillSummaryData, bool, error) {
	var data BillSummaryData
	err := db.QueryRow(ctx, `SELECT deposit_count,payout_count,total_deposit_cny::text,
		total_deposit_usdt::text,total_payout_usdt::text
	FROM ledger_period_summaries
	WHERE chat_id=$1 AND day_key=$2 AND period_started_at=$3 AND NOT dirty AND reconciled_at IS NOT NULL`,
		key.ChatID, key.DayKey, key.PeriodStartedAt).Scan(&data.Summary.DepositCount,
		&data.Summary.PayoutCount, &data.Summary.TotalDepositCNY,
		&data.Summary.TotalDepositUSDT, &data.Summary.TotalPayoutUSDT)
	if errors.Is(err, pgx.ErrNoRows) {
		return BillSummaryData{}, false, nil
	}
	if err != nil {
		return BillSummaryData{}, false, err
	}
	data.Source = "incremental"
	return data, true, nil
}

func (s *Store) getLegacyBillSummaryForPeriod(ctx context.Context, key LedgerPeriodSummaryKey, recentLimit int) (BillSummaryData, error) {
	return getLegacyBillSummaryForPeriodUsing(ctx, s.pool, key, recentLimit)
}

func getLegacyBillSummaryForPeriodUsing(ctx context.Context, db ledgerSummaryQueryer, key LedgerPeriodSummaryKey, recentLimit int) (BillSummaryData, error) {
	var data BillSummaryData
	err := db.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE kind='deposit'),
		COUNT(*) FILTER (WHERE kind='payout'),
		COALESCE(SUM(CASE WHEN kind='deposit' AND upper(currency)='CNY' THEN amount::numeric ELSE 0 END),0)::text,
		COALESCE(SUM(CASE WHEN kind='deposit' THEN result_usdt::numeric ELSE 0 END),0)::text,
		COALESCE(SUM(CASE WHEN kind='payout' THEN result_usdt::numeric ELSE 0 END),0)::text
	FROM records WHERE chat_id=$1 AND day_key=$2
		AND (period_started_at=$3 OR (period_started_at='1970-01-01 00:00:00+00'::timestamptz AND created_at >= $3))
		AND deleted_at IS NULL`,
		key.ChatID, key.DayKey, key.PeriodStartedAt).Scan(&data.Summary.DepositCount,
		&data.Summary.PayoutCount, &data.Summary.TotalDepositCNY,
		&data.Summary.TotalDepositUSDT, &data.Summary.TotalPayoutUSDT)
	if err != nil {
		return BillSummaryData{}, err
	}
	data.Records, err = listRecentRecordsForPeriodUsing(ctx, db, key, recentLimit)
	return data, err
}

func (s *Store) listRecentRecordsForPeriod(ctx context.Context, key LedgerPeriodSummaryKey, limit int) ([]Record, error) {
	return listRecentRecordsForPeriodUsing(ctx, s.pool, key, limit)
}

func listRecentRecordsForPeriodUsing(ctx context.Context, db ledgerSummaryQueryer, key LedgerPeriodSummaryKey, limit int) ([]Record, error) {
	rows, err := db.Query(ctx, `SELECT `+recordSelectColumns+` FROM (
		(SELECT `+recordSelectColumns+` FROM records
		 WHERE chat_id=$1 AND day_key=$2
		   AND (period_started_at=$3 OR (period_started_at='1970-01-01 00:00:00+00'::timestamptz AND created_at >= $3))
		   AND deleted_at IS NULL AND kind='deposit'
		 ORDER BY id DESC LIMIT $4)
		UNION ALL
		(SELECT `+recordSelectColumns+` FROM records
		 WHERE chat_id=$1 AND day_key=$2
		   AND (period_started_at=$3 OR (period_started_at='1970-01-01 00:00:00+00'::timestamptz AND created_at >= $3))
		   AND deleted_at IS NULL AND kind='payout'
		 ORDER BY id DESC LIMIT $4)
	) recent ORDER BY id ASC`, key.ChatID, key.DayKey, key.PeriodStartedAt, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []Record
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) loadAndRepairLedgerPeriodSummary(ctx context.Context, key LedgerPeriodSummaryKey, recentLimit int, now time.Time) (BillSummaryData, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BillSummaryData{}, false, err
	}
	defer rollback(ctx, tx)
	if err := lockLedgerPeriod(ctx, tx, key); err != nil {
		return BillSummaryData{}, false, err
	}
	actual, err := getLegacyBillSummaryForPeriodUsing(ctx, tx, key, recentLimit)
	if err != nil {
		return BillSummaryData{}, false, err
	}
	matched, err := replaceLedgerPeriodSummaryUsing(ctx, tx, key, actual.Summary, now)
	if err != nil {
		return BillSummaryData{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return BillSummaryData{}, false, err
	}
	return actual, matched, nil
}

func replaceLedgerPeriodSummaryUsing(ctx context.Context, db ledgerSummaryDB, key LedgerPeriodSummaryKey, actual RecordDaySummary, now time.Time) (bool, error) {
	previous, ok, err := readHealthyLedgerSummaryUsing(ctx, db, key)
	if err != nil {
		return false, err
	}
	matched := ok && ledgerSummariesEqual(previous.Summary, actual)
	_, err = db.Exec(ctx, `INSERT INTO ledger_period_summaries(
		chat_id,day_key,period_started_at,deposit_count,payout_count,total_deposit_cny,
		total_deposit_usdt,total_payout_usdt,high_watermark_id,dirty,reconciled_at,updated_at)
	VALUES($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8::numeric,
		COALESCE((SELECT MAX(id) FROM records WHERE chat_id=$1 AND day_key=$2
			AND (period_started_at=$3 OR (period_started_at='1970-01-01 00:00:00+00'::timestamptz AND created_at >= $3))),0),FALSE,$9,$9)
	ON CONFLICT(chat_id,day_key,period_started_at) DO UPDATE SET
		deposit_count=excluded.deposit_count,payout_count=excluded.payout_count,
		total_deposit_cny=excluded.total_deposit_cny,total_deposit_usdt=excluded.total_deposit_usdt,
		total_payout_usdt=excluded.total_payout_usdt,high_watermark_id=excluded.high_watermark_id,
		dirty=FALSE,reconciled_at=excluded.reconciled_at,updated_at=excluded.updated_at`,
		key.ChatID, key.DayKey, key.PeriodStartedAt, actual.DepositCount,
		actual.PayoutCount, actual.TotalDepositCNY, actual.TotalDepositUSDT,
		actual.TotalPayoutUSDT, now)
	return matched, err
}

func ledgerSummariesEqual(a, b RecordDaySummary) bool {
	return a.DepositCount == b.DepositCount && a.PayoutCount == b.PayoutCount &&
		numericEqual(a.TotalDepositCNY, b.TotalDepositCNY) &&
		numericEqual(a.TotalDepositUSDT, b.TotalDepositUSDT) &&
		numericEqual(a.TotalPayoutUSDT, b.TotalPayoutUSDT)
}

func numericEqual(a, b string) bool {
	ra, oka := new(big.Rat).SetString(strings.TrimSpace(a))
	rb, okb := new(big.Rat).SetString(strings.TrimSpace(b))
	return oka && okb && ra.Cmp(rb) == 0
}

func (s *Store) ReconcileLedgerSummaries(ctx context.Context, afterID int64, limit int, now time.Time) (LedgerSummaryReconcileStats, error) {
	_ = afterID
	if err := s.discoverLedgerSummaryKeys(ctx, ledgerSummaryBackfillBatchSize, now); err != nil {
		return LedgerSummaryReconcileStats{}, err
	}
	if limit < 1 || limit > 1000 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx, `SELECT chat_id,day_key,period_started_at
		FROM ledger_period_summaries
		ORDER BY dirty DESC,reconciled_at ASC NULLS FIRST,updated_at ASC,chat_id,day_key,period_started_at
		LIMIT $1`, limit)
	if err != nil {
		return LedgerSummaryReconcileStats{}, err
	}
	defer rows.Close()
	stats := LedgerSummaryReconcileStats{}
	keys := make([]LedgerPeriodSummaryKey, 0, limit)
	for rows.Next() {
		var key LedgerPeriodSummaryKey
		if err := rows.Scan(&key.ChatID, &key.DayKey, &key.PeriodStartedAt); err != nil {
			return stats, err
		}
		stats.Scanned++
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	for _, key := range keys {
		_, matched, err := s.loadAndRepairLedgerPeriodSummary(ctx, key, 1, now)
		if err != nil {
			return stats, err
		}
		if matched {
			stats.Unchanged++
		} else {
			stats.Corrected++
		}
	}
	return stats, nil
}

// discoverLedgerSummaryKeys advances a persisted record-ID cursor once. It
// only creates dirty period placeholders; reconciliation below performs one
// authoritative aggregate per period instead of one per record batch.
func (s *Store) discoverLedgerSummaryKeys(ctx context.Context, limit int, now time.Time) error {
	if limit < 1 {
		limit = ledgerSummaryBackfillBatchSize
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	var lastRecordID int64
	var completed bool
	if err := tx.QueryRow(ctx, `SELECT last_record_id,completed_at IS NOT NULL
		FROM ledger_summary_backfill_state WHERE id=1 FOR UPDATE`).Scan(&lastRecordID, &completed); err != nil {
		return err
	}
	if completed {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM records WHERE id>$1)`, lastRecordID).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return tx.Commit(ctx)
		}
	}
	var batchLast, batchCount int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(id),0),COUNT(*) FROM (
		SELECT id FROM records WHERE id>$1 ORDER BY id ASC LIMIT $2
	) batch`, lastRecordID, limit).Scan(&batchLast, &batchCount); err != nil {
		return err
	}
	if batchCount > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_period_summaries(
			chat_id,day_key,period_started_at,high_watermark_id,dirty,updated_at
		) WITH normalized AS (
			SELECT r.id,r.chat_id,r.day_key,CASE
				WHEN r.period_started_at>$3::timestamptz THEN r.period_started_at
				WHEN g.active_day_key=r.day_key AND g.active_period_started_at>$3::timestamptz
					AND r.created_at>=g.active_period_started_at THEN g.active_period_started_at
				ELSE $3::timestamptz
			END AS period_started_at
			FROM (SELECT * FROM records WHERE id>$1 ORDER BY id ASC LIMIT $2) r
			LEFT JOIN groups g ON g.chat_id=r.chat_id
		)
		SELECT chat_id,day_key,period_started_at,MAX(id),TRUE,$4::timestamptz
		FROM normalized GROUP BY chat_id,day_key,period_started_at
		ON CONFLICT(chat_id,day_key,period_started_at) DO UPDATE SET
			dirty=ledger_period_summaries.dirty OR ledger_period_summaries.high_watermark_id<excluded.high_watermark_id,
			updated_at=CASE
				WHEN ledger_period_summaries.high_watermark_id<excluded.high_watermark_id THEN excluded.updated_at
				ELSE ledger_period_summaries.updated_at
			END`, lastRecordID, limit, ledgerPeriodEpoch, now); err != nil {
			return err
		}
	}
	completedAt := any(nil)
	if batchCount < int64(limit) {
		completedAt = now
	}
	if _, err := tx.Exec(ctx, `UPDATE ledger_summary_backfill_state
		SET last_record_id=GREATEST(last_record_id,$1),completed_at=$2,updated_at=$3 WHERE id=1`,
		batchLast, completedAt, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
