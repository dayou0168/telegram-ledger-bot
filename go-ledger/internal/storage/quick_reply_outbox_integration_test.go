package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresQuickReplyOutboxAtomicLifecycle(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "quick_reply_outbox")
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
	stream := fmt.Sprintf("quick-atomic-%d", now.UnixNano())
	item := TelegramInboxUpdate{UpdateID: 100, Payload: []byte(`{"update_id":100}`), Lane: "ledger", RouteKey: "private:7001"}
	if _, err := storeA.PersistTelegramUpdateBatch(ctx, stream, []TelegramInboxUpdate{item}, now); err != nil {
		t.Fatal(err)
	}
	claimed, err := storeA.ClaimTelegramUpdates(ctx, stream, "ledger", "inbox-owner", 1, 8, time.Minute, now)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim inbox=%v err=%v", inboxIDs(claimed), err)
	}
	if _, err := storeA.pool.Exec(ctx, `INSERT INTO telegram_quick_reply_outbox(
		stream_key,inbox_update_id,dedupe_key,actor_user_id,source_chat_id,source_message_id,
		target_chat_id,target_message_id,state_version_update_id,status,attempts,next_attempt_at,
		lease_owner,last_error,created_at,updated_at
	) VALUES('other-stream',999,'quick-conflict',999,999,999,-999,999,999,'pending',0,$1,'','',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	bad := QuickReplyOutboxInsert{DedupeKey: "quick-conflict", ActorUserID: 7001, SourceChatID: 7001,
		SourceMessageID: 501, TargetChatID: -9001, TargetMessageID: 601}
	ok, err := storeA.CommitTelegramPrivateStateHandledAndQuickReply(ctx, claimed[0], "wrong-owner", 7001, -1,
		[]byte(`{"Mode":"quick_reply"}`), true, &bad, now.Add(time.Millisecond))
	if !errors.Is(err, ErrTelegramInboxLeaseLost) || ok {
		t.Fatalf("wrong owner transaction ok=%v err=%v", ok, err)
	}
	if _, found, err := storeA.GetTelegramPrivateRouteState(ctx, stream, 7001); err != nil || found {
		t.Fatalf("wrong owner transaction leaked private state found=%v err=%v", found, err)
	}

	ok, err = storeA.CommitTelegramPrivateStateHandledAndQuickReply(ctx, claimed[0], "inbox-owner", 7001, -1,
		[]byte(`{"Mode":"quick_reply"}`), true, &bad, now.Add(time.Millisecond))
	if err == nil || ok {
		t.Fatalf("conflicting transaction ok=%v err=%v", ok, err)
	}
	if _, found, err := storeA.GetTelegramPrivateRouteState(ctx, stream, 7001); err != nil || found {
		t.Fatalf("failed transaction leaked private state found=%v err=%v", found, err)
	}
	var handled bool
	if err := storeA.pool.QueryRow(ctx, `SELECT handled_at IS NOT NULL FROM telegram_update_inbox WHERE stream_key=$1 AND update_id=100`, stream).Scan(&handled); err != nil || handled {
		t.Fatalf("failed transaction leaked handled=%v err=%v", handled, err)
	}

	if _, err := storeA.pool.Exec(ctx, `CREATE FUNCTION quick_reply_skip_handled_update() RETURNS trigger
		LANGUAGE plpgsql AS $$ BEGIN IF NEW.handled_at IS NOT NULL THEN RETURN NULL; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := storeA.pool.Exec(ctx, `CREATE TRIGGER quick_reply_skip_handled_update
		BEFORE UPDATE OF handled_at ON telegram_update_inbox
		FOR EACH ROW EXECUTE FUNCTION quick_reply_skip_handled_update()`); err != nil {
		t.Fatal(err)
	}
	zeroRow := bad
	zeroRow.DedupeKey = "quick-zero-row"
	ok, err = storeA.CommitTelegramPrivateStateHandledAndQuickReply(ctx, claimed[0], "inbox-owner", 7001, -1,
		[]byte(`{"Mode":"quick_reply"}`), true, &zeroRow, now.Add(2*time.Millisecond))
	if !errors.Is(err, ErrTelegramInboxLeaseLost) || ok {
		t.Fatalf("zero-row transaction ok=%v err=%v", ok, err)
	}
	if _, err := storeA.pool.Exec(ctx, `DROP TRIGGER quick_reply_skip_handled_update ON telegram_update_inbox;
		DROP FUNCTION quick_reply_skip_handled_update()`); err != nil {
		t.Fatal(err)
	}
	if _, found, err := storeA.GetTelegramPrivateRouteState(ctx, stream, 7001); err != nil || found {
		t.Fatalf("zero-row transaction leaked private state found=%v err=%v", found, err)
	}
	if _, found, err := storeA.GetQuickReplyOutboxByDedupe(ctx, zeroRow.DedupeKey); err != nil || found {
		t.Fatalf("zero-row transaction leaked outbox found=%v err=%v", found, err)
	}
	if err := storeA.pool.QueryRow(ctx, `SELECT handled_at IS NOT NULL FROM telegram_update_inbox WHERE stream_key=$1 AND update_id=100`, stream).Scan(&handled); err != nil || handled {
		t.Fatalf("zero-row transaction leaked handled=%v err=%v", handled, err)
	}

	good := bad
	good.DedupeKey = "quick-valid"
	ok, err = storeA.CommitTelegramPrivateStateHandledAndQuickReply(ctx, claimed[0], "inbox-owner", 7001, -1,
		[]byte(`{"Mode":"quick_reply"}`), true, &good, now.Add(3*time.Millisecond))
	if err != nil || !ok {
		t.Fatalf("atomic commit ok=%v err=%v", ok, err)
	}
	state, found, err := storeB.GetTelegramPrivateRouteState(ctx, stream, 7001)
	if err != nil || !found || !state.HasState || state.VersionUpdateID != 100 {
		t.Fatalf("committed state=%+v found=%v err=%v", state, found, err)
	}
	outbox, found, err := storeB.GetQuickReplyOutboxByDedupe(ctx, good.DedupeKey)
	if err != nil || !found || outbox.Status != "pending" || outbox.InboxUpdateID != 100 {
		t.Fatalf("committed outbox=%+v found=%v err=%v", outbox, found, err)
	}
	ok, err = storeA.CommitTelegramPrivateStateHandledAndQuickReply(ctx, claimed[0], "inbox-owner", 7001, -1,
		[]byte(`{"Mode":"quick_reply"}`), true, &good, now.Add(3*time.Millisecond))
	if err != nil || ok {
		t.Fatalf("replayed commit ok=%v err=%v", ok, err)
	}
	var validCount int64
	if err := storeA.pool.QueryRow(ctx, `SELECT COUNT(*) FROM telegram_quick_reply_outbox WHERE dedupe_key='quick-valid'`).Scan(&validCount); err != nil || validCount != 1 {
		t.Fatalf("dedupe count=%d err=%v", validCount, err)
	}
	if _, err := storeA.pool.Exec(ctx, `UPDATE telegram_quick_reply_outbox
		SET status='sent',completed_at=$1,updated_at=$1 WHERE dedupe_key IN ('quick-valid','quick-conflict')`, now); err != nil {
		t.Fatal(err)
	}

	var outboxSequence int64 = 1000
	insertOutbox := func(dedupe string, actor, stateVersion int64, status string, created, completed time.Time) int64 {
		t.Helper()
		outboxSequence++
		var id int64
		var completedArg any
		if !completed.IsZero() {
			completedArg = completed
		}
		err := storeA.pool.QueryRow(ctx, `INSERT INTO telegram_quick_reply_outbox(
			stream_key,inbox_update_id,dedupe_key,actor_user_id,source_chat_id,source_message_id,
			target_chat_id,target_message_id,state_version_update_id,status,attempts,next_attempt_at,
			lease_owner,last_error,created_at,updated_at,completed_at
		) VALUES($1,$2,$3,$4,$4,$2,$5,$2,$6,$7,0,$8,'','',$8,$8,$9) RETURNING id`,
			"state-machine", outboxSequence, dedupe, actor, -actor, stateVersion, status, created, completedArg).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	firstID := insertOutbox("order-a1", 8001, 201, "pending", now, time.Time{})
	secondID := insertOutbox("order-a2", 8001, 202, "pending", now, time.Time{})
	otherID := insertOutbox("order-b1", 8002, 301, "pending", now, time.Time{})
	firstClaims, err := storeA.ClaimQuickReplyOutbox(ctx, "sender-a", 10, 8, 100*time.Millisecond, now.Add(time.Second))
	if err != nil || len(firstClaims) != 2 || !quickReplyIDsContain(firstClaims, firstID) || !quickReplyIDsContain(firstClaims, otherID) || quickReplyIDsContain(firstClaims, secondID) {
		t.Fatalf("first claims=%v err=%v", quickReplyOutboxIDs(firstClaims), err)
	}
	if ok, err := storeA.MarkQuickReplyOutboxSent(ctx, otherID, "sender-a", now.Add(1050*time.Millisecond)); err != nil || !ok {
		t.Fatalf("mark other sent=%v err=%v", ok, err)
	}
	if items, err := storeB.ClaimQuickReplyOutbox(ctx, "sender-b", 10, 8, time.Minute, now.Add(1050*time.Millisecond)); err != nil || len(items) != 0 {
		t.Fatalf("active lease reclaimed=%v err=%v", quickReplyOutboxIDs(items), err)
	}
	reclaimed, err := storeB.ClaimQuickReplyOutbox(ctx, "sender-b", 1, 8, time.Minute, now.Add(1200*time.Millisecond))
	if err != nil || len(reclaimed) != 1 || reclaimed[0].ID != firstID {
		t.Fatalf("reclaimed=%v err=%v", quickReplyOutboxIDs(reclaimed), err)
	}
	if ok, err := storeA.MarkQuickReplyOutboxSent(ctx, firstID, "sender-a", now.Add(1201*time.Millisecond)); err != nil || ok {
		t.Fatalf("stale owner sent=%v err=%v", ok, err)
	}
	if ok, err := storeB.MarkQuickReplyOutboxSent(ctx, firstID, "sender-b", now.Add(1201*time.Millisecond)); err != nil || !ok {
		t.Fatalf("new owner sent=%v err=%v", ok, err)
	}
	next, err := storeB.ClaimQuickReplyOutbox(ctx, "sender-b", 10, 8, time.Minute, now.Add(1202*time.Millisecond))
	if err != nil || len(next) != 1 || next[0].ID != secondID {
		t.Fatalf("ordered successor=%v err=%v", quickReplyOutboxIDs(next), err)
	}

	if _, err := storeA.pool.Exec(ctx, `INSERT INTO telegram_private_route_states(
		stream_key,user_id,state_json,has_state,version_update_id,updated_at
	) VALUES('state-machine',9001,'{"Mode":"group"}',TRUE,500,$1)`, now); err != nil {
		t.Fatal(err)
	}
	staleID := insertOutbox("cancel-stale", 9001, 499, "pending", now, time.Time{})
	staleClaim, err := storeA.ClaimQuickReplyOutbox(ctx, "cancel-owner", 1, 8, time.Minute, now.Add(2*time.Second))
	if err != nil || len(staleClaim) != 1 || staleClaim[0].ID != staleID {
		t.Fatalf("stale cancel claim=%v err=%v", quickReplyOutboxIDs(staleClaim), err)
	}
	cancelled, cleared, err := storeA.CancelQuickReplyOutboxRevoked(ctx, staleID, "cancel-owner", "revoked", now.Add(2*time.Second))
	if err != nil || !cancelled || cleared {
		t.Fatalf("stale cancel cancelled=%v cleared=%v err=%v", cancelled, cleared, err)
	}
	state, found, err = storeA.GetTelegramPrivateRouteState(ctx, "state-machine", 9001)
	if err != nil || !found || !state.HasState || state.VersionUpdateID != 500 {
		t.Fatalf("stale cancel damaged state=%+v found=%v err=%v", state, found, err)
	}
	exactID := insertOutbox("cancel-exact", 9001, 500, "pending", now, time.Time{})
	exactClaim, err := storeA.ClaimQuickReplyOutbox(ctx, "cancel-owner", 1, 8, time.Minute, now.Add(3*time.Second))
	if err != nil || len(exactClaim) != 1 || exactClaim[0].ID != exactID {
		t.Fatalf("exact cancel claim=%v err=%v", quickReplyOutboxIDs(exactClaim), err)
	}
	cancelled, cleared, err = storeA.CancelQuickReplyOutboxRevoked(ctx, exactID, "cancel-owner", "revoked", now.Add(3*time.Second))
	if err != nil || !cancelled || !cleared {
		t.Fatalf("exact cancel cancelled=%v cleared=%v err=%v", cancelled, cleared, err)
	}

	old := now.Add(-80 * time.Hour)
	insertOutbox("cleanup-sent", 9101, 1, "sent", old, old)
	insertOutbox("cleanup-cancelled", 9102, 1, "cancelled", old, old)
	insertOutbox("cleanup-dead", 9103, 1, "dead", old, old)
	insertOutbox("cleanup-pending", 9104, 1, "pending", old, time.Time{})
	insertOutbox("cleanup-recent", 9105, 1, "sent", now, now)
	removed, err := storeA.CleanupQuickReplyOutbox(ctx, now.Add(-72*time.Hour), 100)
	if err != nil || removed != 3 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	var remaining int64
	if err := storeA.pool.QueryRow(ctx, `SELECT COUNT(*) FROM telegram_quick_reply_outbox WHERE dedupe_key LIKE 'cleanup-%'`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("cleanup remaining=%d err=%v", remaining, err)
	}
}

func quickReplyOutboxIDs(items []QuickReplyOutbox) []int64 {
	ids := make([]int64, len(items))
	for i := range items {
		ids[i] = items[i].ID
	}
	return ids
}

func quickReplyIDsContain(items []QuickReplyOutbox, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
