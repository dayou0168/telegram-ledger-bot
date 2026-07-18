package storage

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresBroadcastDeliveryStateMigrationUpgradesV2417AndIsIdempotent(t *testing.T) {
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
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.schema_migrations(version,applied_at) VALUES('2.4.17-broadcast-reply-preferences',NOW())`); err != nil {
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
