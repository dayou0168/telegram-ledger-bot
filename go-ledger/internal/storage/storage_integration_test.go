package storage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresConcurrentOpenSerializesMigration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for schema setup: %v", err)
	}
	defer admin.Close(context.Background())

	schema := fmt.Sprintf("migration_race_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	}()

	migrationURL, err := url.Parse(dsn)
	if err != nil || migrationURL.Scheme == "" {
		t.Fatalf("TEST_DATABASE_URL must be a PostgreSQL URL: %q: %v", dsn, err)
	}
	query := migrationURL.Query()
	query.Set("search_path", schema)
	migrationURL.RawQuery = query.Encode()

	const openers = 4
	start := make(chan struct{})
	stores := make(chan *Store, openers)
	errs := make(chan error, openers)
	var wg sync.WaitGroup
	for i := 0; i < openers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			store, err := Open(ctx, migrationURL.String())
			if err != nil {
				errs <- err
				return
			}
			stores <- store
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(stores)
	for store := range stores {
		store.Close()
	}
	for err := range errs {
		t.Errorf("concurrent Open: %v", err)
	}
	if t.Failed() {
		return
	}

	var versions int
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version IN ('2.1.0', '2.2.0', '2.3.0', '2.4.1', '2.4.2')",
	).Scan(&versions); err != nil {
		t.Fatalf("query migration versions: %v", err)
	}
	if versions != 5 {
		t.Fatalf("migration versions = %d, want 5", versions)
	}
}

func TestPostgresLedgerClearTicketSecurity(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	storeA, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store A: %v", err)
	}
	defer storeA.Close()
	storeB, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store B: %v", err)
	}
	defer storeB.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := now.UnixNano()
	chatID := int64(-910000000000 - suffix%1000000)
	requesterID := int64(710000000000 + suffix%1000000)
	otherUserID := requesterID + 1
	dayKey := now.Format("2006-01-02")
	if err := storeA.EnsureGroup(ctx, chatID, "clear ticket integration", now); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	if err := storeA.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, now); err != nil {
		t.Fatalf("start period: %v", err)
	}
	group, err := storeA.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	recordID, err := storeA.InsertRecord(ctx, Record{
		ChatID: chatID, DayKey: dayKey, Kind: "deposit", Currency: "CNY", Amount: "1",
		Rate: "1", FeeRate: "0", ResultUSDT: "1", ActorUserID: requesterID,
		CreatedAt: group.ActivePeriodStartedAt.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("insert record: %v", err)
	}
	ticket := LedgerClearTicket{
		TokenHash: "clear-59-" + fmt.Sprint(suffix), ChatID: chatID, RequestedByUserID: requesterID,
		DayKey: dayKey, ActivePeriodStartedAt: group.ActivePeriodStartedAt,
		ExpiresAt: now.Add(60 * time.Second), CreatedAt: now,
	}
	if err := storeA.CreateLedgerClearTicket(ctx, ticket); err != nil {
		t.Fatalf("create ticket: %v", err)
	}
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, otherUserID, now.Add(10*time.Second)); err != nil || got.Status != LedgerClearTicketWrongUser {
		t.Fatalf("other user result = %+v, %v", got, err)
	}
	if record, ok, err := storeA.GetRecord(ctx, recordID); err != nil || !ok || record.DeletedAt != nil {
		t.Fatalf("other user changed record = %+v, ok=%t err=%v", record, ok, err)
	}
	if got, err := storeB.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, requesterID, now.Add(59*time.Second)); err != nil || got.Status != LedgerClearTicketApplied || got.DeletedCount != 1 {
		t.Fatalf("second instance 59s result = %+v, %v", got, err)
	}
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, requesterID, now.Add(59*time.Second)); err != nil || got.Status != LedgerClearTicketConsumed {
		t.Fatalf("repeat result = %+v, %v", got, err)
	}

	expired := ticket
	expired.TokenHash = "clear-61-" + fmt.Sprint(suffix)
	if err := storeA.CreateLedgerClearTicket(ctx, expired); err != nil {
		t.Fatalf("create expiring ticket: %v", err)
	}
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, expired.TokenHash, chatID, requesterID, now.Add(61*time.Second)); err != nil || got.Status != LedgerClearTicketExpired {
		t.Fatalf("61s result = %+v, %v", got, err)
	}

	oldPeriod := ticket
	oldPeriod.TokenHash = "clear-period-" + fmt.Sprint(suffix)
	if err := storeA.CreateLedgerClearTicket(ctx, oldPeriod); err != nil {
		t.Fatalf("create old-period ticket: %v", err)
	}
	if err := storeA.SetGroupActivePeriod(ctx, chatID, false, "", "", now.Add(2*time.Minute)); err != nil {
		t.Fatalf("stop period: %v", err)
	}
	if err := storeA.SetGroupActivePeriod(ctx, chatID, true, dayKey, dayKey, now.Add(2*time.Minute+time.Second)); err != nil {
		t.Fatalf("restart period: %v", err)
	}
	if got, err := storeB.ConsumeLedgerClearTicketAndDelete(ctx, oldPeriod.TokenHash, chatID, requesterID, now.Add(30*time.Second)); err != nil || got.Status != LedgerClearTicketPeriodChanged {
		t.Fatalf("old period result = %+v, %v", got, err)
	}
}

func TestPostgresBroadcastGroupRenameDeleteKeepsPermissionsConsistent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	suffix := now.UnixNano()
	chatID := int64(-920000000000 - suffix%1000000)
	userID := int64(720000000000 + suffix%1000000)
	oldName := fmt.Sprintf("integration-old-%d", suffix)
	newName := fmt.Sprintf("integration-new-%d", suffix)
	if err := store.EnsureGroup(ctx, chatID, "permission group", now); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	if err := store.UpsertGlobalOperator(ctx, userID, "primary", 0, userID+100, "integration", now); err != nil {
		t.Fatalf("upsert operator: %v", err)
	}
	if err := store.UpsertBroadcastGroup(ctx, oldName, userID, now); err != nil {
		t.Fatalf("upsert broadcast group: %v", err)
	}
	if _, err := store.AddChatsToBroadcastGroup(ctx, oldName, []int64{chatID}, now); err != nil {
		t.Fatalf("add group chat: %v", err)
	}
	if err := store.AddBroadcastPermission(ctx, userID, "group", 0, oldName, userID+100, now); err != nil {
		t.Fatalf("add permission: %v", err)
	}
	if renamed, affected, err := store.RenameBroadcastGroup(ctx, oldName, newName, userID+100, now.Add(time.Second)); err != nil || !renamed || len(affected) != 1 || affected[0] != userID {
		t.Fatalf("rename = %t affected=%v err=%v", renamed, affected, err)
	}
	permissions, err := store.ListBroadcastPermissions(ctx)
	if err != nil {
		t.Fatalf("list permissions: %v", err)
	}
	foundNew := false
	for _, permission := range permissions {
		if permission.UserID == userID && permission.Target == "group" {
			if permission.GroupName == oldName {
				t.Fatalf("old group permission remains: %+v", permission)
			}
			if permission.GroupName == newName {
				foundNew = true
			}
		}
	}
	if !foundNew {
		t.Fatal("renamed permission was not migrated")
	}
	if deleted, affected, err := store.DeleteBroadcastGroupManaged(ctx, newName, userID+100, now.Add(2*time.Second)); err != nil || !deleted || len(affected) != 1 || affected[0] != userID {
		t.Fatalf("delete = %t affected=%v err=%v", deleted, affected, err)
	}
	permissions, err = store.ListBroadcastPermissions(ctx)
	if err != nil {
		t.Fatalf("list permissions after delete: %v", err)
	}
	for _, permission := range permissions {
		if permission.UserID == userID && permission.Target == "group" && permission.GroupName == newName {
			t.Fatalf("deleted group permission remains: %+v", permission)
		}
	}
	oldDeliveryID, err := store.InsertBroadcastDelivery(ctx, BroadcastDelivery{
		OperatorUserID: userID, SourceChatID: userID, SourceMessageID: 1,
		TargetChatID: chatID, TargetTitle: "permission group", TargetMessageID: suffix%1000000 + 100,
		Mode: "chat", TargetName: "permission group", CreatedAt: now.Add(-169 * time.Hour),
	})
	if err != nil {
		t.Fatalf("insert old delivery: %v", err)
	}
	recentDeliveryID, err := store.InsertBroadcastDelivery(ctx, BroadcastDelivery{
		OperatorUserID: userID, SourceChatID: userID, SourceMessageID: 2,
		TargetChatID: chatID, TargetTitle: "permission group", TargetMessageID: suffix%1000000 + 101,
		Mode: "chat", TargetName: "permission group", CreatedAt: now.Add(-167 * time.Hour),
	})
	if err != nil {
		t.Fatalf("insert recent delivery: %v", err)
	}
	if deleted, err := store.CleanupBroadcastDeliveries(ctx, now.Add(-168*time.Hour)); err != nil || deleted < 1 {
		t.Fatalf("cleanup deliveries deleted=%d err=%v", deleted, err)
	}
	if _, ok, err := store.GetBroadcastDelivery(ctx, oldDeliveryID); err != nil || ok {
		t.Fatalf("old delivery remains ok=%t err=%v", ok, err)
	}
	if _, ok, err := store.GetBroadcastDelivery(ctx, recentDeliveryID); err != nil || !ok {
		t.Fatalf("valid delivery was removed ok=%t err=%v", ok, err)
	}
}

func TestPostgresPrivateCleanupScopeAndReschedule(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID := int64(730000000000 + now.UnixNano()%1000000)
	if err := store.EnsurePrivateCleanupCarrier(ctx, userID, userID, "cleanup integration", now); err != nil {
		t.Fatalf("ensure carrier: %v", err)
	}
	if saved, err := store.SetBroadcastOperatorPrivateCleanupSettings(ctx, userID, PrivateCleanupSettings{
		Enabled: true, BotDeleteAfter: 300, Scope: DefaultPrivateCleanupScope(),
	}, now); err != nil || !saved {
		t.Fatalf("save initial settings=%t err=%v", saved, err)
	}
	for i, category := range []string{"broadcast", "menu"} {
		dueAt := now.Add(300 * time.Second)
		if err := store.RecordPrivateChatMessage(ctx, PrivateChatMessage{
			OperatorUserID: userID, ChatID: userID, MessageID: int64(95000 + i),
			Direction: "outgoing", Category: category, CleanupAfterSeconds: 300,
			DueAt: &dueAt, CreatedAt: now,
		}); err != nil {
			t.Fatalf("record %s message: %v", category, err)
		}
	}
	if saved, err := store.SetBroadcastOperatorPrivateCleanupSettings(ctx, userID, PrivateCleanupSettings{
		Enabled: true, BotDeleteAfter: 60, Scope: "broadcast",
	}, now.Add(10*time.Second)); err != nil || !saved {
		t.Fatalf("save narrowed settings=%t err=%v", saved, err)
	}
	var broadcastDue time.Time
	var menuDeleted *time.Time
	if err := store.pool.QueryRow(ctx, `SELECT due_at FROM private_chat_messages WHERE chat_id=$1 AND message_id=95000`, userID).Scan(&broadcastDue); err != nil {
		t.Fatalf("read broadcast due: %v", err)
	}
	if !broadcastDue.Equal(now.Add(60 * time.Second)) {
		t.Fatalf("broadcast due = %v, want %v", broadcastDue, now.Add(60*time.Second))
	}
	if err := store.pool.QueryRow(ctx, `SELECT deleted_at FROM private_chat_messages WHERE chat_id=$1 AND message_id=95001`, userID).Scan(&menuDeleted); err != nil {
		t.Fatalf("read menu deleted: %v", err)
	}
	if menuDeleted == nil {
		t.Fatal("excluded menu message should be closed instead of retried")
	}
}

func TestPostgresStoreBasicFlow(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	suffix := now.UnixNano()
	chatID := -900000000000 - suffix%1000000
	userID := int64(700000000000 + suffix%1000000)
	if userID <= 1<<31-1 {
		t.Fatalf("integration user id %d must exceed PostgreSQL int4", userID)
	}

	claimed, err := store.ClaimUpdate(ctx, suffix, now)
	if err != nil {
		t.Fatalf("claim update: %v", err)
	}
	if !claimed {
		t.Fatalf("first update claim should be true")
	}
	claimed, err = store.ClaimUpdate(ctx, suffix, now)
	if err != nil {
		t.Fatalf("claim duplicate update: %v", err)
	}
	if claimed {
		t.Fatalf("duplicate update claim should be false")
	}

	if err := store.EnsureGroup(ctx, chatID, "Go v2.3 test group", now); err != nil {
		t.Fatalf("ensure group: %v", err)
	}
	user := User{ID: userID, Username: "go23", DisplayName: "Go 2.3"}
	if err := store.TouchUser(ctx, chatID, user, now); err != nil {
		t.Fatalf("touch user: %v", err)
	}
	if err := store.SetGroupOwner(ctx, chatID, user, now); err != nil {
		t.Fatalf("set owner: %v", err)
	}
	ok, err := store.IsOperator(ctx, chatID, userID)
	if err != nil {
		t.Fatalf("is operator: %v", err)
	}
	if !ok {
		t.Fatalf("owner should also be operator")
	}
	ok, err = store.IsGlobalOperator(ctx, userID)
	if err != nil {
		t.Fatalf("is global operator before grant: %v", err)
	}
	if ok {
		t.Fatal("single-group operator should not be a global operator")
	}

	group, err := store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if group.OwnerUserID != userID {
		t.Fatalf("owner mismatch: got %d want %d", group.OwnerUserID, userID)
	}
	if err := store.SetGroupActivePeriod(ctx, chatID, true, "2026-07-06", "2026-07-06", now); err != nil {
		t.Fatalf("set active period: %v", err)
	}
	group, err = store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get active group: %v", err)
	}
	if !group.Active || group.ActiveDayKey != "2026-07-06" || group.ActiveExpiresDayKey != "2026-07-06" || group.ActivePeriodStartedAt.IsZero() {
		t.Fatalf("active period not persisted: %+v", group)
	}
	firstPeriodStart := group.ActivePeriodStartedAt
	if err := store.SetGroupActive(ctx, chatID, false, "", now.Add(time.Second)); err != nil {
		t.Fatalf("stop active period: %v", err)
	}
	restartAt := now.Add(2 * time.Second)
	if err := store.SetGroupActivePeriod(ctx, chatID, true, "2026-07-06", "2026-07-06", restartAt); err != nil {
		t.Fatalf("restart active period: %v", err)
	}
	group, err = store.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get restarted group: %v", err)
	}
	if !group.ActivePeriodStartedAt.After(firstPeriodStart) {
		t.Fatalf("restarted period did not advance start time: %v <= %v", group.ActivePeriodStartedAt, firstPeriodStart)
	}
	now = restartAt

	recordID, err := store.InsertRecord(ctx, Record{
		ChatID:          chatID,
		DayKey:          "2026-07-06",
		Kind:            "deposit",
		Currency:        "CNY",
		Amount:          "100",
		Rate:            "7",
		FeeRate:         "0",
		ResultUSDT:      "14.29",
		ActorUserID:     userID,
		ActorName:       user.DisplayName,
		SourceMessageID: 1001,
		Remark:          "integration",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("insert record: %v", err)
	}
	if recordID == 0 {
		t.Fatalf("record id should be non-zero")
	}
	largeBotMessageID := int64(600000000000 + suffix%1000000)
	dedupeKey := fmt.Sprintf("bigint-message-id-%d", suffix)
	enqueued, err := store.EnqueueNotification(ctx, NotificationOutbox{
		Kind:          "ledger_record",
		DedupeKey:     dedupeKey,
		ChatID:        chatID,
		Text:          "BIGINT message id regression",
		ReferenceKind: "ledger_record",
		ReferenceID:   recordID,
	}, now)
	if err != nil || !enqueued {
		t.Fatalf("enqueue BIGINT message id regression: %v, inserted=%v", err, enqueued)
	}
	var notificationID int64
	if err := store.pool.QueryRow(ctx, `SELECT id FROM notification_outbox WHERE dedupe_key=$1`, dedupeKey).Scan(&notificationID); err != nil {
		t.Fatalf("find BIGINT message id notification: %v", err)
	}
	if err := store.MarkNotificationSent(ctx, notificationID, largeBotMessageID, now); err != nil {
		t.Fatalf("mark notification with BIGINT message id: %v", err)
	}
	record, ok, err := store.GetRecord(ctx, recordID)
	if err != nil || !ok || record.BotMessageID != largeBotMessageID {
		t.Fatalf("record BIGINT bot message id = %d, %v, %v; want %d", record.BotMessageID, ok, err, largeBotMessageID)
	}

	address := "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ"
	if err := store.AddWatch(ctx, userID, address, "watch address", now); err != nil {
		t.Fatalf("add watch: %v", err)
	}
	targets, err := store.ListWatchTargets(ctx)
	if err != nil {
		t.Fatalf("list watch targets: %v", err)
	}
	found := false
	for _, target := range targets {
		if target.OwnerUserID == userID && target.Address == address {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("watch target was not returned")
	}
	count, err := store.CountActiveWatchTargetsForOwner(ctx, userID)
	if err != nil {
		t.Fatalf("count watch targets: %v", err)
	}
	if count != 1 {
		t.Fatalf("active watch target count = %d, want 1", count)
	}

	tokenHash := "ticket-" + time.Now().Format("150405.000000000")
	if err := store.CreateAdminLoginTicket(ctx, tokenHash, userID, "operator", now.Add(time.Minute), now); err != nil {
		t.Fatalf("create admin login ticket: %v", err)
	}
	ticket, ok, err := store.GetAdminLoginTicket(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("get admin login ticket: %v", err)
	}
	if !ok || ticket.UserID != userID || ticket.Role != "operator" {
		t.Fatalf("unexpected admin ticket: ok=%v ticket=%+v", ok, ticket)
	}
	ticket, ok, err = store.ConsumeAdminLoginTicket(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("consume admin login ticket: %v", err)
	}
	if !ok || ticket.UserID != userID || ticket.Role != "operator" {
		t.Fatalf("unexpected consumed admin ticket: ok=%v ticket=%+v", ok, ticket)
	}
	_, ok, err = store.ConsumeAdminLoginTicket(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("consume admin login ticket again: %v", err)
	}
	if ok {
		t.Fatal("admin login ticket should not be consumed twice")
	}
	_, ok, err = store.GetAdminLoginTicket(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("get consumed admin login ticket: %v", err)
	}
	if ok {
		t.Fatal("consumed admin login ticket should not be valid")
	}
	expiredTokenHash := tokenHash + "-expired"
	if err := store.CreateAdminLoginTicket(ctx, expiredTokenHash, userID, "operator", now.Add(-time.Minute), now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("create expired admin login ticket: %v", err)
	}
	_, ok, err = store.GetAdminLoginTicket(ctx, expiredTokenHash, now)
	if err != nil {
		t.Fatalf("get expired admin login ticket: %v", err)
	}
	if ok {
		t.Fatal("expired admin login ticket should not be valid")
	}

	if err := store.AddBroadcastPermission(ctx, userID, "chat", chatID, "", 0, now); err == nil {
		t.Fatal("non-global operator should not receive broadcast permission")
	}
	if err := store.UpsertGlobalOperator(ctx, userID, "primary", 0, 0, "cleanup operator", now); err != nil {
		t.Fatalf("upsert global operator: %v", err)
	}
	if err := store.AddBroadcastPermission(ctx, userID, "chat", chatID, "", 0, now); err != nil {
		t.Fatalf("global operator should receive broadcast permission: %v", err)
	}
	level, ok, err := store.GetGlobalOperatorLevel(ctx, userID)
	if err != nil {
		t.Fatalf("get global operator level: %v", err)
	}
	if !ok || level != "primary" {
		t.Fatalf("global operator level = %q, %v; want primary, true", level, ok)
	}
	globalOperators, err := store.ListGlobalOperators(ctx)
	if err != nil {
		t.Fatalf("list global operators: %v", err)
	}
	foundGlobalOperator := false
	for _, op := range globalOperators {
		if op.UserID == userID && op.Level == "primary" && op.Status == "active" {
			foundGlobalOperator = true
			break
		}
	}
	if !foundGlobalOperator {
		t.Fatal("global operator should be listed")
	}
	secondaryUserID := userID + 1
	if err := store.UpsertGlobalOperator(ctx, secondaryUserID, "secondary", userID, userID, "secondary operator", now); err != nil {
		t.Fatalf("upsert secondary global operator: %v", err)
	}
	secondary, ok, err := store.GetGlobalOperator(ctx, secondaryUserID)
	if err != nil || !ok || secondary.ParentUserID != userID || secondary.Level != "secondary" {
		t.Fatalf("secondary operator = %+v, %v, %v", secondary, ok, err)
	}
	secondaryAudit, err := store.ListPermissionAuditEvents(ctx, secondaryUserID, 10)
	if err != nil {
		t.Fatalf("list secondary permission audit: %v", err)
	}
	foundSecondaryParent := false
	for _, event := range secondaryAudit {
		if event.Action == "created" && event.ParentUserID == userID {
			foundSecondaryParent = true
			break
		}
	}
	if !foundSecondaryParent {
		t.Fatalf("secondary audit did not preserve BIGINT parent %d: %+v", userID, secondaryAudit)
	}
	if err := store.UpsertGlobalOperator(ctx, secondaryUserID+1, "secondary", userID+999, userID, "invalid parent", now); err == nil {
		t.Fatal("secondary with inactive or missing primary parent should fail")
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO global_operators(user_id, level, status, created_by, created_at)
		VALUES($1, 'invalid', 'active', 0, $2)`, secondaryUserID+2, now); err == nil {
		t.Fatal("database should reject invalid global operator level")
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO global_operators(user_id, level, status, created_by, created_at)
		VALUES($1, 'primary', 'unknown', 0, $2)`, secondaryUserID+20, now); err == nil {
		t.Fatal("database should reject invalid global operator status")
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO global_operators(user_id, level, status, created_by, created_at)
		VALUES($1, 'secondary', 'active', 0, $2)`, secondaryUserID+21, now); err == nil {
		t.Fatal("database should reject secondary without parent")
	}
	if err := store.AddBroadcastPermission(ctx, secondaryUserID, "chat", chatID, "", userID, now); err != nil {
		t.Fatalf("add secondary broadcast permission: %v", err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, userID, "chat", chatID, ""); err != nil || !allowed {
		t.Fatalf("primary broadcast scope = %v, %v", allowed, err)
	}
	legacyUserID := secondaryUserID + 3
	if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_operators(user_id, status, created_by, remark, created_at, updated_at)
		VALUES($1, 'active', 0, 'late legacy row', $2, $2)`, legacyUserID, now); err != nil {
		t.Fatalf("insert late legacy broadcast operator: %v", err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("rerun migrate: %v", err)
	}
	if ok, err := store.IsGlobalOperator(ctx, legacyUserID); err != nil || ok {
		t.Fatalf("one-time migration re-created legacy identity: %v, %v", ok, err)
	}
	cleanupMinutes := now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Hour()*60 + now.In(time.FixedZone("Asia/Shanghai", 8*3600)).Minute()
	cleanupTime := time.Date(2000, 1, 1, cleanupMinutes/60, cleanupMinutes%60, 0, 0, time.UTC).Format("15:04")
	saved, err := store.SetBroadcastOperatorPrivateCleanup(ctx, userID, true, cleanupTime, "", now)
	if err != nil {
		t.Fatalf("set private cleanup: %v", err)
	}
	if !saved {
		t.Fatal("private cleanup setting should save")
	}
	if err := store.RecordPrivateChatMessage(ctx, PrivateChatMessage{
		OperatorUserID: userID,
		ChatID:         userID,
		MessageID:      81001,
		Direction:      "incoming",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("record incoming private chat message: %v", err)
	}
	if err := store.RecordPrivateChatMessage(ctx, PrivateChatMessage{
		OperatorUserID: userID,
		ChatID:         userID,
		MessageID:      81002,
		Direction:      "outgoing",
		CreatedAt:      now,
	}); err != nil {
		t.Fatalf("record outgoing private chat message: %v", err)
	}
	privateMessages, err := store.ListPrivateChatMessagesForCleanup(ctx, userID, 10)
	if err != nil {
		t.Fatalf("list private cleanup messages: %v", err)
	}
	if len(privateMessages) != 2 {
		t.Fatalf("private cleanup message count = %d, want 2", len(privateMessages))
	}
	for _, privateMessage := range privateMessages {
		if privateMessage.Direction == "" || privateMessage.LastError != "" {
			t.Fatalf("unexpected private message metadata: %+v", privateMessage)
		}
	}
	cleanupTargets, err := store.ListDuePrivateCleanupTargets(ctx, cleanupMinutes, "1999-01-01")
	if err != nil {
		t.Fatalf("list due private cleanup targets: %v", err)
	}
	foundCleanupTarget := false
	for _, target := range cleanupTargets {
		if target.UserID == userID {
			foundCleanupTarget = true
		}
	}
	if !foundCleanupTarget {
		t.Fatal("private cleanup target should be due")
	}
	deliveryID, err := store.InsertBroadcastDelivery(ctx, BroadcastDelivery{
		OperatorUserID:  userID,
		SourceChatID:    userID,
		SourceMessageID: 91001,
		TargetChatID:    chatID,
		TargetTitle:     "Go v2.3 test group",
		TargetMessageID: 91002,
		Mode:            "chat",
		TargetName:      "Go v2.3 test group",
		CreatedAt:       now,
	})
	if err != nil {
		t.Fatalf("insert broadcast delivery before cleanup: %v", err)
	}
	if err := store.MarkPrivateChatMessageCleanup(ctx, privateMessages[0].ID, "", now); err != nil {
		t.Fatalf("mark private cleanup success: %v", err)
	}
	if err := store.MarkPrivateChatMessageCleanup(ctx, privateMessages[1].ID, "delete failed", now); err != nil {
		t.Fatalf("mark private cleanup failure: %v", err)
	}
	if err := store.MarkPrivateCleanupRun(ctx, userID, "1999-01-01", now); err != nil {
		t.Fatalf("mark private cleanup run: %v", err)
	}
	privateMessages, err = store.ListPrivateChatMessagesForCleanup(ctx, userID, 10)
	if err != nil {
		t.Fatalf("list private cleanup messages after mark: %v", err)
	}
	if len(privateMessages) != 0 {
		t.Fatalf("private cleanup messages should not be retried, got %d", len(privateMessages))
	}
	if _, ok, err := store.GetBroadcastDelivery(ctx, deliveryID); err != nil {
		t.Fatalf("get broadcast delivery after private cleanup: %v", err)
	} else if !ok {
		t.Fatal("private cleanup should not delete broadcast deliveries")
	}
	disabled, err := store.DisableGlobalOperator(ctx, userID, 0, now)
	if err != nil {
		t.Fatalf("disable global operator: %v", err)
	}
	if !disabled {
		t.Fatal("global operator should disable")
	}
	ok, err = store.IsGlobalOperator(ctx, userID)
	if err != nil {
		t.Fatalf("is global operator after disable: %v", err)
	}
	if ok {
		t.Fatal("disabled global operator should not be active")
	}
	if ok, err := store.IsGlobalOperator(ctx, secondaryUserID); err != nil || ok {
		t.Fatalf("secondary should be disabled with primary: %v, %v", ok, err)
	}
	permissions, err := store.ListBroadcastPermissions(ctx)
	if err != nil {
		t.Fatalf("list permissions after disable: %v", err)
	}
	for _, permission := range permissions {
		if permission.UserID == userID || permission.UserID == secondaryUserID {
			t.Fatalf("disabled operator retained broadcast permission: %+v", permission)
		}
	}
	if err := store.UpsertGlobalOperator(ctx, userID, "primary", 0, 9999, "reenabled primary", now.Add(time.Second)); err != nil {
		t.Fatalf("reenable primary: %v", err)
	}
	permissions, err = store.ListBroadcastPermissions(ctx)
	if err != nil {
		t.Fatalf("list permissions after reenable: %v", err)
	}
	for _, permission := range permissions {
		if permission.UserID == userID {
			t.Fatalf("reenable restored old broadcast permission: %+v", permission)
		}
	}
	auditEvents, err := store.ListPermissionAuditEvents(ctx, userID, 20)
	if err != nil {
		t.Fatalf("list permission audit: %v", err)
	}
	actions := map[string]bool{}
	for _, event := range auditEvents {
		actions[event.Action] = true
	}
	for _, action := range []string{"created", "disabled", "reenabled"} {
		if !actions[action] {
			t.Fatalf("permission audit missing %q: %+v", action, auditEvents)
		}
	}
	if len(auditEvents) == 0 {
		t.Fatal("permission audit should not be empty")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE permission_audit_events SET action='tampered' WHERE id=$1`, auditEvents[0].ID); err == nil {
		t.Fatal("permission audit event should be immutable")
	}

	inserted, err := store.RecordChainNotification(ctx, userID, address, "txhash-"+time.Now().Format("150405.000000000"), "income", now.UnixMilli(), now)
	if err != nil {
		t.Fatalf("record chain notification: %v", err)
	}
	if !inserted {
		t.Fatalf("first chain notification should insert")
	}
}

func TestPostgresRecordKeysetAndCurrentPeriodClear(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	chatID := -910000000000 - now.UnixNano()%1000000
	dayKey := "2026-07-12"
	periodStart := now.Add(-time.Minute)
	for i := 0; i < 7; i++ {
		createdAt := periodStart.Add(time.Duration(i) * time.Second)
		if i == 0 {
			createdAt = periodStart.Add(-time.Minute)
		}
		_, err := store.InsertRecord(ctx, Record{
			ChatID: chatID, DayKey: dayKey, Kind: "deposit", Currency: "CNY",
			Amount: fmt.Sprintf("%d", i+1), Rate: "1", FeeRate: "0", ResultUSDT: fmt.Sprintf("%d", i+1),
			ActorUserID: 1, ActorName: "actor", SubjectName: "subject", CreatedAt: createdAt,
		})
		if err != nil {
			t.Fatalf("insert record %d: %v", i, err)
		}
	}

	newest, err := store.ListRecordsForDayPage(ctx, chatID, dayKey, RecordFilter{}, 0, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(newest.Records) != 3 || !newest.HasOlder || newest.HasNewer {
		t.Fatalf("newest page = %+v", newest)
	}
	older, err := store.ListRecordsForDayPage(ctx, chatID, dayKey, RecordFilter{}, newest.Records[0].ID, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Records) != 3 || !older.HasOlder || !older.HasNewer || older.Records[2].ID >= newest.Records[0].ID {
		t.Fatalf("older page = %+v", older)
	}
	newerAgain, err := store.ListRecordsForDayPage(ctx, chatID, dayKey, RecordFilter{}, 0, older.Records[len(older.Records)-1].ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(newerAgain.Records) != 3 || newerAgain.Records[0].ID != newest.Records[0].ID {
		t.Fatalf("newer page did not return adjacent records: %+v", newerAgain)
	}

	count, err := store.CountRecordsForPeriod(ctx, chatID, dayKey, periodStart)
	if err != nil || count != 6 {
		t.Fatalf("current period count = %d, err = %v", count, err)
	}
	deleted, err := store.SoftDeleteRecordsForPeriod(ctx, chatID, dayKey, periodStart, now.Add(time.Minute))
	if err != nil || deleted != 6 {
		t.Fatalf("current period deleted = %d, err = %v", deleted, err)
	}
	remaining, err := store.ListRecordsForDayPage(ctx, chatID, dayKey, RecordFilter{}, 0, 0, 10)
	if err != nil || len(remaining.Records) != 1 {
		t.Fatalf("records from earlier period should remain: %d, err = %v", len(remaining.Records), err)
	}
}

func TestChainWatcherConcurrentSourcesCreateOneDelivery(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	suffix := fmt.Sprint(time.Now().UnixNano())
	event := ChainWatcherEvent{EventID: "event-" + suffix, TxHash: "tx-" + suffix, From: "A", To: "B", Value: "1", EventIndex: "0", Source: "realtime"}
	delivery := ChainWatcherMatchedEvent{DeliveryID: "delivery-" + suffix, EventID: event.EventID, BotID: "bot", ChatID: 1, OwnerUserID: 1, WatchAddress: "B", Direction: "income"}
	var wg sync.WaitGroup
	results := make(chan int, 4)
	for _, source := range []string{"realtime", "expand", "catchup", "fallback"} {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			copyEvent := event
			copyEvent.Source = source
			inserted, insertErr := store.RecordChainWatcherMatches(ctx, copyEvent, []ChainWatcherMatchedEvent{delivery}, time.Now())
			if insertErr != nil {
				t.Errorf("source %s: %v", source, insertErr)
				return
			}
			results <- inserted
		}(source)
	}
	wg.Wait()
	close(results)
	total := 0
	for inserted := range results {
		total += inserted
	}
	if total != 1 {
		t.Fatalf("inserted deliveries = %d, want 1", total)
	}

	second := event
	second.EventID += "-log1"
	second.EventIndex = "1"
	secondDelivery := delivery
	secondDelivery.EventID, secondDelivery.DeliveryID = second.EventID, delivery.DeliveryID+"-log1"
	inserted, err := store.RecordChainWatcherMatches(ctx, second, []ChainWatcherMatchedEvent{secondDelivery}, time.Now())
	if err != nil || inserted != 1 {
		t.Fatalf("second log event = %d/%v", inserted, err)
	}
}

func TestFallbackLeaseElectsSingleLeader(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	leaseName := fmt.Sprintf("test-fallback-%d", time.Now().UnixNano())
	now := time.Now().UTC()
	first, leader, err := store.AcquireChainWatcherFallbackLease(ctx, leaseName, "bot-a", "FALLBACK_ACTIVE", 10*time.Second, now)
	if err != nil || !leader || first.HolderID != "bot-a" {
		t.Fatalf("first lease = %+v/%v/%v", first, leader, err)
	}
	second, leader, err := store.AcquireChainWatcherFallbackLease(ctx, leaseName, "bot-b", "FALLBACK_ACTIVE", 10*time.Second, now.Add(time.Second))
	if err != nil || leader || second.HolderID != "bot-a" {
		t.Fatalf("competing lease = %+v/%v/%v", second, leader, err)
	}
	third, leader, err := store.AcquireChainWatcherFallbackLease(ctx, leaseName, "bot-b", "FALLBACK_ACTIVE", 10*time.Second, now.Add(11*time.Second))
	if err != nil || !leader || third.HolderID != "bot-b" {
		t.Fatalf("expired lease takeover = %+v/%v/%v", third, leader, err)
	}
}

func TestChainWatcherCursorSurvivesNewStoreInstance(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	first, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().UnixMilli()
	eventID := fmt.Sprintf("restart-cursor-%d", timestamp)
	if err := first.AdvanceChainWatcherWatermark(ctx, timestamp, eventID, "catchup", time.Now().UTC()); err != nil {
		first.Close()
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	watermark, err := second.GetChainWatcherWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if watermark.Timestamp != timestamp || watermark.TxHash != eventID {
		t.Fatalf("watermark after reopen = %+v, want %d/%s", watermark, timestamp, eventID)
	}
}

func TestAddressWatchBaselineStartsAtRegistrationAndResetsAfterReactivation(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := time.Now().UnixNano()
	address := fmt.Sprintf("TBaseline%d", owner)
	first := time.Now().UTC().Truncate(time.Millisecond)
	if err := store.AddWatch(ctx, owner, address, "", first); err != nil {
		t.Fatal(err)
	}
	target, ok, err := store.GetWatchTarget(ctx, owner, address)
	if err != nil || !ok || target.BaselineTimestamp != first.UnixMilli() {
		t.Fatalf("first baseline = %+v/%v/%v", target, ok, err)
	}
	if _, err := store.RemoveWatch(ctx, owner, address, first.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	second := first.Add(2 * time.Second)
	if err := store.AddWatch(ctx, owner, address, "", second); err != nil {
		t.Fatal(err)
	}
	target, ok, err = store.GetWatchTarget(ctx, owner, address)
	if err != nil || !ok || target.BaselineTimestamp != second.UnixMilli() {
		t.Fatalf("reactivated baseline = %+v/%v/%v", target, ok, err)
	}
}

func TestChainWatcherGapLeaseFencingRejectsExpiredWorker(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	from := now.UnixMilli() + now.UnixNano()%1000
	if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "window", Source: "watcher", Priority: 2,
		FromTimestamp: from, ToTimestamp: from + 1000,
	}, now); err != nil {
		t.Fatal(err)
	}
	first, ok, err := store.ClaimChainWatcherGap(ctx, "worker-a", "watcher", time.Second, now)
	if err != nil || !ok {
		t.Fatalf("first claim = %+v/%v/%v", first, ok, err)
	}
	second, ok, err := store.ClaimChainWatcherGap(ctx, "worker-b", "watcher", time.Second, now.Add(2*time.Second))
	if err != nil || !ok || second.ID != first.ID || second.LeaseGeneration <= first.LeaseGeneration {
		t.Fatalf("second claim = %+v/%v/%v", second, ok, err)
	}
	if completed, err := store.CompleteChainWatcherGap(ctx, first.ID, first.LeaseGeneration, first.LeaseOwner, now.Add(2*time.Second)); err != nil || completed {
		t.Fatalf("expired worker completion = %v/%v", completed, err)
	}
	if completed, err := store.CompleteChainWatcherGap(ctx, second.ID, second.LeaseGeneration, second.LeaseOwner, now.Add(2*time.Second)); err != nil || !completed {
		t.Fatalf("current worker completion = %v/%v", completed, err)
	}
	if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "window", Source: "watcher", Priority: 1,
		FromTimestamp: from, ToTimestamp: from + 1000,
	}, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	reopened, ok, err := store.ClaimChainWatcherGap(ctx, "worker-c", "watcher", time.Second, now.Add(3*time.Second))
	if err != nil || !ok || reopened.ID != first.ID || reopened.LeaseGeneration <= second.LeaseGeneration {
		t.Fatalf("reopened completed gap = %+v/%v/%v", reopened, ok, err)
	}
	_, _ = store.CompleteChainWatcherGap(ctx, reopened.ID, reopened.LeaseGeneration, reopened.LeaseOwner, now.Add(3*time.Second))
}
