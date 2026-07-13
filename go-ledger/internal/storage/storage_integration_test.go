package storage

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

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
