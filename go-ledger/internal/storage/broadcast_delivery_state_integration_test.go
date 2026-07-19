package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresBroadcastDeliveryStateMigrationUpgradesObserverSchemaAndIsIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, admin, schema := postgresTestSchema(t, ctx, dsn, "broadcast_delivery_upgrade")
	if _, err := admin.Exec(ctx, `CREATE TABLE `+schema+`.schema_migrations(version TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.schema_migrations(version,applied_at) VALUES($1,NOW())`, operatorMessageObserversMigrationVersion); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		store, err := Open(ctx, migrationURL)
		if err != nil {
			t.Fatalf("Open attempt %d: %v", attempt+1, err)
		}
		store.Close()
	}
	var marker, targetTable, upstreamTable bool
	if err := admin.QueryRow(ctx, `SELECT
		EXISTS(SELECT 1 FROM `+schema+`.schema_migrations WHERE version=$1),
		to_regclass($2) IS NOT NULL,to_regclass($3) IS NOT NULL`,
		broadcastDeliveryStateMigrationVersion, schema+".telegram_broadcast_targets", schema+".broadcast_upstream_messages").Scan(
		&marker, &targetTable, &upstreamTable); err != nil {
		t.Fatal(err)
	}
	if !marker || !targetTable || !upstreamTable {
		t.Fatalf("migration marker=%t target=%t upstream=%t", marker, targetTable, upstreamTable)
	}
}

func TestPostgresPersistentBroadcastTargetTracksGroupChangesRenameDeleteAndRestart(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_target")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	chatA, chatB := int64(-980001), int64(-980002)
	if err := store.EnsureGroup(ctx, chatA, "A", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatB, "B", now); err != nil {
		t.Fatal(err)
	}
	groupName := fmt.Sprintf("target-%d", now.UnixNano())
	if err := store.UpsertBroadcastGroup(ctx, groupName, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddChatsToBroadcastGroup(ctx, groupName, []int64{chatA}, now); err != nil {
		t.Fatal(err)
	}
	group, ok, err := store.GetBroadcastGroup(ctx, groupName)
	if err != nil || !ok || group.ID <= 0 {
		t.Fatalf("group=%+v ok=%t err=%v", group, ok, err)
	}
	target := TelegramBroadcastTarget{StreamKey: "bot:test", UserID: 101, Mode: "group", GroupID: group.ID, TargetName: groupName, NotifyAll: true}
	if err := store.UpsertTelegramBroadcastTarget(ctx, target, now); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, ok, err := store.GetTelegramBroadcastTarget(ctx, target.StreamKey, target.UserID)
	if err != nil || !ok || restored.GroupID != group.ID || !restored.NotifyAll {
		t.Fatalf("restored=%+v ok=%t err=%v", restored, ok, err)
	}
	if _, err := store.AddChatsToBroadcastGroup(ctx, groupName, []int64{chatB}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	members, err := store.ListBroadcastGroupChats(ctx, groupName)
	if err != nil || len(members) != 2 {
		t.Fatalf("members=%+v err=%v", members, err)
	}
	renamed := groupName + "-renamed"
	if changed, _, err := store.RenameBroadcastGroup(ctx, groupName, renamed, 1, true, now.Add(2*time.Second)); err != nil || !changed {
		t.Fatalf("rename changed=%t err=%v", changed, err)
	}
	restored, ok, err = store.GetTelegramBroadcastTarget(ctx, target.StreamKey, target.UserID)
	if err != nil || !ok || restored.TargetName != renamed || restored.GroupID <= 0 {
		t.Fatalf("renamed target=%+v ok=%t err=%v", restored, ok, err)
	}
	if deleted, _, err := store.DeleteBroadcastGroupManaged(ctx, renamed, 1, true, now.Add(3*time.Second)); err != nil || !deleted {
		t.Fatalf("delete=%t err=%v", deleted, err)
	}
	if _, ok, err := store.GetTelegramBroadcastTarget(ctx, target.StreamKey, target.UserID); err != nil || ok {
		t.Fatalf("deleted group target still exists ok=%t err=%v", ok, err)
	}
	chatTarget := TelegramBroadcastTarget{StreamKey: target.StreamKey, UserID: target.UserID, Mode: "chat", ChatID: chatA, TargetName: "A"}
	if err := store.UpsertTelegramBroadcastTarget(ctx, chatTarget, now.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := store.GetTelegramBroadcastTarget(ctx, target.StreamKey, target.UserID); err != nil || !ok || got.ChatID != chatA {
		t.Fatalf("long-lived chat target=%+v ok=%t err=%v", got, ok, err)
	}
	if err := store.DeleteTelegramBroadcastTarget(ctx, target.StreamKey, target.UserID); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresBroadcastMediaOutboxIsIdempotentMapsReplyAndRetainsPending(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_media_outbox")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	item := NotificationOutbox{Kind: "broadcast_upstream_copy", DedupeKey: "upstream:1", ChatID: 200,
		PayloadType: "photo", FileID: "photo-file-id", Caption: "caption", Priority: 1}
	inserted, upstream, err := store.EnqueueBroadcastUpstreamMessage(ctx, item, 100, 100, 10, 200, now)
	if err != nil || !inserted || upstream.ID <= 0 {
		t.Fatalf("enqueue inserted=%t upstream=%+v err=%v", inserted, upstream, err)
	}
	inserted, duplicate, err := store.EnqueueBroadcastUpstreamMessage(ctx, item, 100, 100, 10, 200, now)
	if err != nil || inserted || duplicate.ID != upstream.ID {
		t.Fatalf("duplicate inserted=%t upstream=%+v err=%v", inserted, duplicate, err)
	}
	claimed, err := store.ClaimDueNotifications(ctx, 10, 8, now)
	if err != nil || len(claimed) != 1 || claimed[0].PayloadType != "photo" || claimed[0].FileID != "photo-file-id" || claimed[0].Caption != "caption" {
		t.Fatalf("claimed=%+v err=%v", claimed, err)
	}
	if err := store.MarkNotificationSent(ctx, claimed[0].ID, 777, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	mapped, ok, err := store.GetBroadcastUpstreamMessageByID(ctx, upstream.ID)
	if err != nil || !ok || mapped.TelegramMessageID != 777 {
		t.Fatalf("mapped=%+v ok=%t err=%v", mapped, ok, err)
	}
	if stats, err := store.CleanupNotificationOutbox(ctx, now.Add(2*time.Second), now.Add(-24*time.Hour)); err != nil || stats.SentDeleted != 1 {
		t.Fatalf("outbox cleanup=%+v err=%v", stats, err)
	}
	if inserted, duplicate, err := store.EnqueueBroadcastUpstreamMessage(ctx, item, 100, 100, 10, 200, now.Add(3*time.Second)); err != nil || inserted || duplicate.TelegramMessageID != 777 {
		t.Fatalf("post-cleanup duplicate inserted=%t upstream=%+v err=%v", inserted, duplicate, err)
	}
	reply := NotificationOutbox{Kind: "broadcast_reply_notice", DedupeKey: "reply:1", ChatID: 200,
		PayloadType: "photo", FileID: "reply-photo", Caption: "reply caption", ReplyToUpstreamID: upstream.ID, Priority: 1}
	if inserted, err := store.EnqueueNotification(ctx, reply, now.Add(2*time.Second)); err != nil || !inserted {
		t.Fatalf("enqueue reply inserted=%t err=%v", inserted, err)
	}
	claimed, err = store.ClaimDueNotifications(ctx, 10, 8, now.Add(2*time.Second))
	if err != nil || len(claimed) != 1 || claimed[0].ReplyToUpstreamID != upstream.ID {
		t.Fatalf("reply claimed=%+v err=%v", claimed, err)
	}
	old := now.Add(-200 * time.Hour)
	pendingItem := NotificationOutbox{Kind: "broadcast_upstream_copy", DedupeKey: "upstream:pending", ChatID: 201,
		PayloadType: "text", Text: "pending", Priority: 1}
	if inserted, _, err := store.EnqueueBroadcastUpstreamMessage(ctx, pendingItem, 101, 101, 11, 201, old); err != nil || !inserted {
		t.Fatalf("enqueue pending inserted=%t err=%v", inserted, err)
	}
	if deleted, err := store.CleanupBroadcastUpstreamMessages(ctx, now.Add(-168*time.Hour)); err != nil || deleted != 0 {
		t.Fatalf("pending cleanup deleted=%d err=%v", deleted, err)
	}
}

func TestPostgresBroadcastUpstreamCompanionIsAtomicOrderedAndIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_upstream_companion")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Add(-200 * time.Hour)
	primary := NotificationOutbox{Kind: "broadcast_upstream_copy", DedupeKey: "upstream:context:primary", ChatID: 202,
		PayloadType: "photo", FileID: "original-photo", Caption: "original caption", Priority: 1}
	companion := NotificationOutbox{Kind: "broadcast_upstream_context", DedupeKey: "upstream:context:companion", ChatID: 202,
		PayloadType: "text", Text: "sender and target", Priority: 1}
	inserted, upstream, err := store.EnqueueBroadcastUpstreamMessage(ctx, primary, 102, 102, 12, 202, now, companion)
	if err != nil || !inserted || upstream.ID <= 0 {
		t.Fatalf("enqueue inserted=%t upstream=%+v err=%v", inserted, upstream, err)
	}
	if inserted, duplicate, err := store.EnqueueBroadcastUpstreamMessage(ctx, primary, 102, 102, 12, 202, now, companion); err != nil || inserted || duplicate.ID != upstream.ID {
		t.Fatalf("duplicate inserted=%t upstream=%+v err=%v", inserted, duplicate, err)
	}
	var rows int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notification_outbox WHERE dedupe_key IN ($1,$2)`, primary.DedupeKey, companion.DedupeKey).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("outbox rows=%d err=%v", rows, err)
	}
	claimed, err := store.ClaimDueNotifications(ctx, 10, 8, time.Now().UTC())
	if err != nil || len(claimed) != 1 || claimed[0].DedupeKey != primary.DedupeKey {
		t.Fatalf("first claim=%+v err=%v", claimed, err)
	}
	if deleted, err := store.CleanupBroadcastUpstreamMessages(ctx, time.Now().UTC().Add(-168*time.Hour)); err != nil || deleted != 0 {
		t.Fatalf("primary sending cleanup deleted=%d err=%v", deleted, err)
	}
	if err := store.MarkNotificationSent(ctx, claimed[0].ID, 888, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.CleanupBroadcastUpstreamMessages(ctx, time.Now().UTC().Add(-168*time.Hour)); err != nil || deleted != 0 {
		t.Fatalf("companion pending cleanup deleted=%d err=%v", deleted, err)
	}
	claimed, err = store.ClaimDueNotifications(ctx, 10, 8, time.Now().UTC())
	if err != nil || len(claimed) != 1 || claimed[0].DedupeKey != companion.DedupeKey || claimed[0].ReplyToUpstreamID != upstream.ID {
		t.Fatalf("companion claim=%+v err=%v", claimed, err)
	}

	badPrimary := NotificationOutbox{Kind: "broadcast_upstream_copy", DedupeKey: "upstream:rollback:primary", ChatID: 203,
		PayloadType: "text", Text: "original", Priority: 1}
	badCompanion := NotificationOutbox{Kind: "broadcast_upstream_context", PayloadType: "text", Text: "invalid", Priority: 1}
	if _, _, err := store.EnqueueBroadcastUpstreamMessage(ctx, badPrimary, 103, 103, 13, 203, time.Now().UTC(), badCompanion); err == nil {
		t.Fatal("invalid companion should roll back the transaction")
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM notification_outbox WHERE dedupe_key=$1`, badPrimary.DedupeKey).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rolled-back outbox rows=%d err=%v", rows, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM broadcast_upstream_messages WHERE source_chat_id=$1 AND source_message_id=$2`, int64(103), int64(13)).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rolled-back upstream rows=%d err=%v", rows, err)
	}
}

func TestPostgresBroadcastReplaceSettingDisableClearAndRestart(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_replace_restart")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := store.SaveBroadcastReplaceSetting(ctx, BroadcastReplaceSetting{
		Enabled: true, Text: "fixed", ImageName: "fixed.jpg", ImageData: []byte("image"), UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	setting, err := store.GetBroadcastReplaceSetting(ctx)
	if err != nil || !setting.Enabled || setting.Text != "fixed" || string(setting.ImageData) != "image" {
		t.Fatalf("restored setting=%+v err=%v", setting, err)
	}
	if err := store.SaveBroadcastReplaceSetting(ctx, BroadcastReplaceSetting{Enabled: false, UpdatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setting, err = store.GetBroadcastReplaceSetting(ctx)
	if err != nil || setting.Enabled || setting.Text != "" || setting.ImageName != "" || len(setting.ImageData) != 0 {
		t.Fatalf("cleared setting=%+v err=%v", setting, err)
	}
}

func TestPostgresBroadcastFinalReceiptRetriesAcrossRestartWithoutDuplicates(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_final_receipt")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	item := NotificationOutbox{Kind: "broadcast_result", DedupeKey: "broadcast_result:501:601", ChatID: 501,
		PayloadType: "text", Text: "发送完成", Priority: 1}
	if inserted, err := store.EnqueueNotification(ctx, item, now); err != nil || !inserted {
		t.Fatalf("enqueue inserted=%t err=%v", inserted, err)
	}
	if inserted, err := store.EnqueueNotification(ctx, item, now); err != nil || inserted {
		t.Fatalf("duplicate inserted=%t err=%v", inserted, err)
	}
	claimed, err := store.ClaimDueNotifications(ctx, 10, 8, now)
	if err != nil || len(claimed) != 1 || claimed[0].DedupeKey != item.DedupeKey {
		t.Fatalf("first claim=%+v err=%v", claimed, err)
	}
	retryAt := now.Add(time.Second)
	if err := store.MarkNotificationFailed(ctx, claimed[0].ID, "telegram 429", retryAt, now); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if claimed, err := store.ClaimDueNotifications(ctx, 10, 8, now.Add(500*time.Millisecond)); err != nil || len(claimed) != 0 {
		t.Fatalf("premature retry=%+v err=%v", claimed, err)
	}
	claimed, err = store.ClaimDueNotifications(ctx, 10, 8, retryAt)
	if err != nil || len(claimed) != 1 || claimed[0].ID <= 0 {
		t.Fatalf("restart retry=%+v err=%v", claimed, err)
	}
	if err := store.MarkNotificationSent(ctx, claimed[0].ID, 701, retryAt); err != nil {
		t.Fatal(err)
	}
	if inserted, err := store.EnqueueNotification(ctx, item, retryAt); err != nil || inserted {
		t.Fatalf("post-send duplicate inserted=%t err=%v", inserted, err)
	}
	var count int
	var status string
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*),MIN(status) FROM notification_outbox WHERE dedupe_key=$1`, item.DedupeKey).Scan(&count, &status); err != nil || count != 1 || status != "sent" {
		t.Fatalf("receipt count=%d status=%q err=%v", count, status, err)
	}
}
