package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresTelegramDurableInboxLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "telegram_inbox")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)

	t.Run("persist before offset and legacy compatibility", func(t *testing.T) {
		stream := "persist"
		if _, err := store.pool.Exec(ctx, `INSERT INTO processed_updates(update_id,processed_at) VALUES(10,$1)`, now); err != nil {
			t.Fatal(err)
		}
		offset, err := store.PersistTelegramUpdateBatch(ctx, stream, []TelegramInboxUpdate{
			{UpdateID: 10, Payload: []byte(`{"update_id":10}`), Lane: "ledger", RouteKey: "ledger:-1"},
			{UpdateID: 11, Payload: []byte(`{"update_id":11}`), Lane: "bypass", RouteKey: "bypass:-1:11"},
		}, now)
		if err != nil || offset != 12 {
			t.Fatalf("persist offset=%d err=%v", offset, err)
		}
		var statuses []string
		rows, err := store.pool.Query(ctx, `SELECT status FROM telegram_update_inbox WHERE stream_key=$1 ORDER BY update_id`, stream)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var status string
			if err := rows.Scan(&status); err != nil {
				t.Fatal(err)
			}
			statuses = append(statuses, status)
		}
		rows.Close()
		if fmt.Sprint(statuses) != "[done pending]" {
			t.Fatalf("statuses=%v", statuses)
		}
		if _, err := store.PersistTelegramUpdateBatch(ctx, stream, []TelegramInboxUpdate{
			{UpdateID: 12, Payload: []byte(`{"update_id":12}`), Lane: "ledger", RouteKey: "ledger:-1"},
			{UpdateID: 13, Payload: []byte(`{"update_id":13}`), Lane: "invalid", RouteKey: "ledger:-1"},
		}, now.Add(time.Second)); err == nil {
			t.Fatal("invalid batch unexpectedly advanced")
		}
		if got, err := store.GetTelegramPollOffset(ctx, stream); err != nil || got != 12 {
			t.Fatalf("offset after rollback=%d err=%v", got, err)
		}
		var rolledBack int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_update_inbox WHERE stream_key=$1 AND update_id=12`, stream).Scan(&rolledBack); err != nil || rolledBack != 0 {
			t.Fatalf("rolled-back inbox rows=%d err=%v", rolledBack, err)
		}
	})

	t.Run("restart lease retry ordering and handled ack recovery", func(t *testing.T) {
		stream := "lifecycle"
		items := []TelegramInboxUpdate{
			{UpdateID: 20, Payload: []byte(`{"update_id":20}`), Lane: "ledger", RouteKey: "ledger:-2"},
			{UpdateID: 21, Payload: []byte(`{"update_id":21}`), Lane: "ledger", RouteKey: "ledger:-2"},
			{UpdateID: 22, Payload: []byte(`{"update_id":22}`), Lane: "ledger", RouteKey: "ledger:-3"},
		}
		if _, err := store.PersistTelegramUpdateBatch(ctx, stream, items, now); err != nil {
			t.Fatal(err)
		}
		restarted, err := Open(ctx, migrationURL)
		if err != nil {
			t.Fatal(err)
		}
		defer restarted.Close()
		if offset, err := restarted.GetTelegramPollOffset(ctx, stream); err != nil || offset != 23 {
			t.Fatalf("restarted offset=%d err=%v", offset, err)
		}
		claimed, err := restarted.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-a", 10, 4, 30*time.Millisecond, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(claimed) != 2 || claimed[0].UpdateID != 20 || claimed[1].UpdateID != 22 {
			t.Fatalf("initial claims=%v", inboxIDs(claimed))
		}
		status, err := restarted.RetryTelegramUpdate(ctx, stream, 20, "owner-a", 4, now.Add(20*time.Millisecond), now, fmt.Errorf("handler failed"))
		if err != nil || status != "retry" {
			t.Fatalf("retry status=%q err=%v", status, err)
		}
		if ok, err := restarted.MarkTelegramUpdateHandled(ctx, stream, 22, "owner-a", now); err != nil || !ok {
			t.Fatalf("mark handled=%v err=%v", ok, err)
		}
		time.Sleep(40 * time.Millisecond)
		reclaimed, err := restarted.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-b", 10, 4, time.Minute, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		if len(reclaimed) != 2 || reclaimed[0].UpdateID != 20 || reclaimed[1].UpdateID != 22 || reclaimed[1].HandledAt == nil {
			t.Fatalf("reclaimed=%v handled=%v", inboxIDs(reclaimed), len(reclaimed) == 2 && reclaimed[1].HandledAt != nil)
		}
		for _, item := range reclaimed {
			if item.HandledAt == nil {
				if ok, err := restarted.MarkTelegramUpdateHandled(ctx, stream, item.UpdateID, "owner-b", time.Now().UTC()); err != nil || !ok {
					t.Fatalf("mark %d handled=%v err=%v", item.UpdateID, ok, err)
				}
			}
			if ok, err := restarted.CompleteTelegramUpdate(ctx, stream, item.UpdateID, "owner-b", time.Now().UTC()); err != nil || !ok {
				t.Fatalf("complete %d ok=%v err=%v", item.UpdateID, ok, err)
			}
		}
		next, err := restarted.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-c", 10, 4, time.Minute, time.Now().UTC())
		if err != nil || len(next) != 1 || next[0].UpdateID != 21 {
			t.Fatalf("ordered successor=%v err=%v", inboxIDs(next), err)
		}
	})

	t.Run("bounded retry becomes visible dead letter", func(t *testing.T) {
		stream := "dead"
		if _, err := store.PersistTelegramUpdateBatch(ctx, stream, []TelegramInboxUpdate{{
			UpdateID: 50, Payload: []byte(`{"update_id":50}`), Lane: "bypass", RouteKey: "bypass:-5:50",
		}}, now); err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= 2; attempt++ {
			claimed, err := store.ClaimTelegramUpdates(ctx, stream, "bypass", "dead-owner", 1, 2, time.Minute, now.Add(time.Duration(attempt)*time.Second))
			if err != nil || len(claimed) != 1 {
				t.Fatalf("attempt %d claim=%v err=%v", attempt, inboxIDs(claimed), err)
			}
			status, err := store.RetryTelegramUpdate(ctx, stream, 50, "dead-owner", 2,
				now.Add(time.Duration(attempt)*time.Second), now.Add(time.Duration(attempt)*time.Second), fmt.Errorf("attempt %d", attempt))
			if err != nil {
				t.Fatal(err)
			}
			want := "retry"
			if attempt == 2 {
				want = "dead"
			}
			if status != want {
				t.Fatalf("attempt %d status=%q want=%q", attempt, status, want)
			}
		}
		if claimed, err := store.ClaimTelegramUpdates(ctx, stream, "bypass", "other", 1, 2, time.Minute, now.Add(time.Hour)); err != nil || len(claimed) != 0 {
			t.Fatalf("dead reclaim=%v err=%v", inboxIDs(claimed), err)
		}
		stats, err := store.TelegramInboxStats(ctx, stream, now.Add(time.Hour))
		if err != nil || stats.Dead != 1 {
			t.Fatalf("dead stats=%+v err=%v", stats, err)
		}
	})

	t.Run("lane isolation stats dead and done-only cleanup", func(t *testing.T) {
		stream := "lanes"
		items := []TelegramInboxUpdate{{UpdateID: 100, Payload: []byte(`{"update_id":100}`), Lane: "ledger", RouteKey: "ledger:-9"}}
		for id := int64(101); id < 140; id++ {
			items = append(items, TelegramInboxUpdate{UpdateID: id, Payload: []byte(fmt.Sprintf(`{"update_id":%d}`, id)), Lane: "bypass", RouteKey: fmt.Sprintf("bypass:-9:%d", id)})
		}
		if _, err := store.PersistTelegramUpdateBatch(ctx, stream, items, now.Add(-96*time.Hour)); err != nil {
			t.Fatal(err)
		}
		ledger, err := store.ClaimTelegramUpdates(ctx, stream, "ledger", "ledger-owner", 1, 2, time.Minute, now)
		if err != nil || len(ledger) != 1 || ledger[0].UpdateID != 100 {
			t.Fatalf("ledger under bypass flood=%v err=%v", inboxIDs(ledger), err)
		}
		if ok, err := store.MarkTelegramUpdateHandled(ctx, stream, 100, "ledger-owner", now.Add(-80*time.Hour)); err != nil || !ok {
			t.Fatal(err)
		}
		if ok, err := store.CompleteTelegramUpdate(ctx, stream, 100, "ledger-owner", now.Add(-80*time.Hour)); err != nil || !ok {
			t.Fatal(err)
		}
		removed, err := store.CleanupDoneTelegramUpdates(ctx, stream, now.Add(-72*time.Hour), 100)
		if err != nil || removed != 1 {
			t.Fatalf("cleanup removed=%d err=%v", removed, err)
		}
		var bypassCount int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM telegram_update_inbox WHERE stream_key=$1 AND lane='bypass'`, stream).Scan(&bypassCount); err != nil || bypassCount != 39 {
			t.Fatalf("pending bypass count=%d err=%v", bypassCount, err)
		}
		stats, err := store.TelegramInboxStats(ctx, stream, now)
		if err != nil || stats.Pending != 39 || stats.Processing != 0 {
			t.Fatalf("stats=%+v err=%v", stats, err)
		}
	})
}

func inboxIDs(items []TelegramInboxUpdate) []int64 {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.UpdateID)
	}
	return ids
}

func TestPostgresUndoOutboxDedupePreventsReplayDeletingNextRecord(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "undo_replay")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	chatID := int64(-88001)
	period := now.Add(-time.Hour)
	insert := func(created time.Time) int64 {
		id, err := store.InsertRecord(ctx, Record{
			ChatID: chatID, DayKey: "2026-07-15", PeriodStartedAt: period,
			Kind: "deposit", Currency: "CNY", Amount: "100", Rate: "1", FeeRate: "0", ResultUSDT: "100",
			SubjectUserID: 1, SubjectName: "subject", ActorUserID: 2, ActorName: "actor", CreatedAt: created,
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	firstID := insert(now.Add(-time.Minute))
	secondID := insert(now)
	receipt := NotificationOutbox{
		Kind: "ledger_bill", DedupeKey: "undo-replay:-88001:77", ChatID: chatID,
		Text: "undo", ReferenceKind: "ledger_record", Priority: 0,
	}
	first, outboxID, deleted, err := store.SoftDeleteRecordWithSummaryAndOutbox(ctx, chatID, firstID, now.Add(time.Second), "deposit", period, false, receipt)
	if err != nil || !deleted || first.ID != firstID || outboxID == 0 {
		t.Fatalf("first undo record=%d outbox=%d deleted=%v err=%v", first.ID, outboxID, deleted, err)
	}
	replayed, replayOutboxID, replayedResult, err := store.SoftDeleteRecordWithSummaryAndOutbox(ctx, chatID, secondID, now.Add(2*time.Second), "deposit", period, false, receipt)
	if err != nil || !replayedResult || replayed.ID != firstID || replayOutboxID != outboxID {
		t.Fatalf("replay record=%d outbox=%d deleted=%v err=%v", replayed.ID, replayOutboxID, replayedResult, err)
	}
	var secondStillActive bool
	if err := store.pool.QueryRow(ctx, `SELECT deleted_at IS NULL FROM records WHERE id=$1`, secondID).Scan(&secondStillActive); err != nil {
		t.Fatal(err)
	}
	if !secondStillActive {
		t.Fatal("replayed undo deleted the next record")
	}
}

func TestPostgresTelegramPrivateRouteStateCASAndLeaseOwnership(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "private_route_state")
	storeA, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stream := "private-cas"
	userID := int64(99101)
	items := []TelegramInboxUpdate{
		{UpdateID: 200, Payload: []byte(`{"update_id":200}`), Lane: "ledger", RouteKey: "private:99101"},
		{UpdateID: 201, Payload: []byte(`{"update_id":201}`), Lane: "ledger", RouteKey: "private:99101"},
		{UpdateID: 202, Payload: []byte(`{"update_id":202}`), Lane: "ledger", RouteKey: "private:99101"},
	}
	if _, err := storeA.PersistTelegramUpdateBatch(ctx, stream, items, now); err != nil {
		t.Fatal(err)
	}
	first, err := storeA.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-a", 10, 5, 100*time.Millisecond, now)
	if err != nil || len(first) != 1 || first[0].UpdateID != 200 {
		t.Fatalf("first claim=%v err=%v", inboxIDs(first), err)
	}
	if ok, err := storeA.CommitTelegramPrivateStateAndMarkHandled(ctx, first[0], "owner-a", userID, -1,
		[]byte(`{"Mode":"quick_reply","NotifyAll":false}`), true, now.Add(time.Millisecond)); err != nil || !ok {
		t.Fatalf("first state commit=%v err=%v", ok, err)
	}
	if ok, err := storeA.CompleteTelegramUpdate(ctx, stream, 200, "owner-a", now.Add(2*time.Millisecond)); err != nil || !ok {
		t.Fatalf("first completion=%v err=%v", ok, err)
	}
	state, found, err := storeB.GetTelegramPrivateRouteState(ctx, stream, userID)
	if err != nil || !found || !state.HasState || state.VersionUpdateID != 200 {
		t.Fatalf("state after restart=%+v found=%v err=%v", state, found, err)
	}
	second, err := storeB.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-b", 10, 5, 100*time.Millisecond, now.Add(3*time.Millisecond))
	if err != nil || len(second) != 1 || second[0].UpdateID != 201 {
		t.Fatalf("second claim=%v err=%v", inboxIDs(second), err)
	}
	if ok, err := storeB.CommitTelegramPrivateStateAndMarkHandled(ctx, second[0], "owner-b", userID, -1,
		[]byte(`{"Mode":"stale"}`), true, now.Add(4*time.Millisecond)); err != nil || ok {
		t.Fatalf("stale CAS=%v err=%v", ok, err)
	}
	if ok, err := storeB.CommitTelegramPrivateStateAndMarkHandled(ctx, second[0], "owner-b", userID, 200,
		[]byte(`{}`), false, now.Add(5*time.Millisecond)); err != nil || !ok {
		t.Fatalf("second state commit=%v err=%v", ok, err)
	}
	if ok, err := storeB.CompleteTelegramUpdate(ctx, stream, 201, "owner-b", now.Add(6*time.Millisecond)); err != nil || !ok {
		t.Fatalf("second completion=%v err=%v", ok, err)
	}
	state, found, err = storeA.GetTelegramPrivateRouteState(ctx, stream, userID)
	if err != nil || !found || state.HasState || state.VersionUpdateID != 201 {
		t.Fatalf("tombstone state=%+v found=%v err=%v", state, found, err)
	}
	third, err := storeA.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-old", 1, 5, 40*time.Millisecond, now.Add(7*time.Millisecond))
	if err != nil || len(third) != 1 || third[0].UpdateID != 202 {
		t.Fatalf("third claim=%v err=%v", inboxIDs(third), err)
	}
	reclaimAt := now.Add(60 * time.Millisecond)
	reclaimed, err := storeB.ClaimTelegramUpdates(ctx, stream, "ledger", "owner-new", 1, 5, time.Minute, reclaimAt)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].UpdateID != 202 {
		t.Fatalf("reclaimed claim=%v err=%v", inboxIDs(reclaimed), err)
	}
	if ok, err := storeA.RenewTelegramUpdateLease(ctx, stream, 202, "owner-old", time.Minute, reclaimAt); err != nil || ok {
		t.Fatalf("stale renew=%v err=%v", ok, err)
	}
	if ok, err := storeA.MarkTelegramUpdateHandled(ctx, stream, 202, "owner-old", reclaimAt); err != nil || ok {
		t.Fatalf("stale handled=%v err=%v", ok, err)
	}
	if ok, err := storeA.CommitTelegramPrivateStateAndMarkHandled(ctx, third[0], "owner-old", userID, 201,
		[]byte(`{"Mode":"stale"}`), true, reclaimAt); !errors.Is(err, ErrTelegramInboxLeaseLost) || ok {
		t.Fatalf("stale private commit=%v err=%v", ok, err)
	}
	if ok, err := storeB.CommitTelegramPrivateStateAndMarkHandled(ctx, reclaimed[0], "owner-new", userID, 201,
		[]byte(`{"Mode":"broadcast","NotifyAll":true}`), true, reclaimAt.Add(time.Millisecond)); err != nil || !ok {
		t.Fatalf("new owner state commit=%v err=%v", ok, err)
	}
	if cleared, err := storeA.ClearTelegramPrivateRouteStateVersion(ctx, stream, userID, 201, reclaimAt.Add(2*time.Millisecond)); err != nil || cleared {
		t.Fatalf("stale state clear=%v err=%v", cleared, err)
	}
	if cleared, err := storeA.ClearTelegramPrivateRouteStateVersion(ctx, stream, userID, 202, reclaimAt.Add(3*time.Millisecond)); err != nil || !cleared {
		t.Fatalf("current state clear=%v err=%v", cleared, err)
	}
	state, found, err = storeB.GetTelegramPrivateRouteState(ctx, stream, userID)
	if err != nil || !found || state.HasState || state.VersionUpdateID != 202 {
		t.Fatalf("cleared state=%+v found=%v err=%v", state, found, err)
	}
}
