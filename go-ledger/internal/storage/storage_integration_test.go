package storage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func postgresTestSchema(t *testing.T, ctx context.Context, dsn, prefix string) (string, *pgx.Conn, string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for schema setup: %v", err)
	}

	schema := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		admin.Close(context.Background())
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
		admin.Close(context.Background())
	})

	migrationURL, err := url.Parse(dsn)
	if err != nil || migrationURL.Scheme == "" {
		t.Fatalf("TEST_DATABASE_URL must be a PostgreSQL URL: %q: %v", dsn, err)
	}
	query := migrationURL.Query()
	query.Set("search_path", schema)
	migrationURL.RawQuery = query.Encode()
	return migrationURL.String(), admin, quotedSchema
}

func TestPostgresConcurrentOpenSerializesMigration(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, admin, quotedSchema := postgresTestSchema(t, ctx, dsn, "migration_race")

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
			store, err := Open(ctx, migrationURL)
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
		"SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version IN ('2.1.0', '2.2.0', '2.3.0', '2.4.1', '2.4.2', '2.4.3', '2.4.3-broadcast-permission-restore', '2.4.4-broadcast-group-ownership', '2.4.5-broadcast-group-owner-transfer')",
	).Scan(&versions); err != nil {
		t.Fatalf("query migration versions: %v", err)
	}
	if versions != 9 {
		t.Fatalf("migration versions = %d, want 9", versions)
	}
}

func TestPostgresQuickReplyOutboxMigrationUpgradesParentMarkerIdempotently(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, admin, quotedSchema := postgresTestSchema(t, ctx, dsn, "quick_reply_upgrade")
	schemaName := strings.Trim(quotedSchema, `"`)

	fresh, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	fresh.Close()
	if _, err := admin.Exec(ctx, "DROP TABLE "+quotedSchema+".telegram_quick_reply_outbox"); err != nil {
		t.Fatalf("drop quick reply outbox fixture: %v", err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM "+quotedSchema+".schema_migrations WHERE version=$1", latestSchemaMigrationVersion); err != nil {
		t.Fatalf("remove child marker: %v", err)
	}
	var parentMarkers int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version=$1",
		telegramPrivateRouteStateMigrationVersion).Scan(&parentMarkers); err != nil || parentMarkers != 1 {
		t.Fatalf("parent marker count=%d err=%v", parentMarkers, err)
	}

	upgraded, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	upgraded.Close()
	reopened, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("idempotent Open: %v", err)
	}
	reopened.Close()

	var tableCount, indexCount, markerCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_tables
		WHERE schemaname=$1 AND tablename='telegram_quick_reply_outbox'`, schemaName).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_indexes WHERE schemaname=$1
		AND indexname IN ('idx_quick_reply_outbox_due','idx_quick_reply_outbox_stream_actor_open',
			'idx_quick_reply_outbox_lease','idx_quick_reply_outbox_cleanup')`, schemaName).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version=$1",
		latestSchemaMigrationVersion).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 1 || indexCount != 4 || markerCount != 1 {
		t.Fatalf("upgrade table=%d indexes=%d marker=%d", tableCount, indexCount, markerCount)
	}
}

func TestPostgresGlobalPermissionEpochNotifiesOtherStore(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "permission_epoch")
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
	uniqueEpoch := time.Now().UnixNano()
	if _, err := storeA.pool.Exec(ctx, `UPDATE permission_epochs
		SET epoch=$1, updated_at=NOW() WHERE scope=$2`, uniqueEpoch, PermissionScopeGlobalOperator); err != nil {
		t.Fatal(err)
	}

	events := make(chan PermissionInvalidation, 4)
	listenCtx, stopListener := context.WithCancel(ctx)
	defer stopListener()
	listenerErr := make(chan error, 1)
	go func() {
		listenerErr <- storeB.ListenPermissionInvalidations(listenCtx, func(event PermissionInvalidation) {
			events <- event
		})
	}()
	var initial PermissionInvalidation
	select {
	case initial = <-events:
	case err := <-listenerErr:
		t.Fatalf("start listener: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not report initial epoch")
	}
	if initial.Scope != PermissionScopeGlobalOperator || initial.Epoch != uniqueEpoch {
		t.Fatalf("initial event = %+v", initial)
	}
	waitForEpoch := func(want int64) PermissionInvalidation {
		t.Helper()
		for {
			select {
			case event := <-events:
				if event.Scope == PermissionScopeGlobalOperator && event.Epoch == want {
					return event
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("notification did not reach epoch %d", want)
			}
		}
	}

	userID := int64(700000000000 + time.Now().UnixNano()%1000000)
	now := time.Now().UTC()
	if err := storeA.UpsertGlobalOperator(ctx, userID, "primary", 0, userID, "epoch integration", now); err != nil {
		t.Fatal(err)
	}
	epoch, err := storeB.GetPermissionEpoch(ctx, PermissionScopeGlobalOperator)
	if err != nil {
		t.Fatal(err)
	}
	changed := waitForEpoch(epoch)
	if changed.Epoch != epoch {
		t.Fatalf("stored epoch = %d; notification = %+v", epoch, changed)
	}
	if disabled, err := storeA.DisableGlobalOperator(ctx, userID, userID, now.Add(time.Second)); err != nil || !disabled {
		t.Fatalf("disable global operator = %v, %v", disabled, err)
	}
	disabledEpoch, err := storeA.GetPermissionEpoch(ctx, PermissionScopeGlobalOperator)
	if err != nil {
		t.Fatal(err)
	}
	if event := waitForEpoch(disabledEpoch); event.Epoch != disabledEpoch {
		t.Fatalf("disable notification = %+v, want epoch %d", event, disabledEpoch)
	}
	if _, err := storeA.pool.Exec(ctx, `DELETE FROM global_operators WHERE user_id=$1`, userID); err != nil {
		t.Fatal(err)
	}
	deletedEpoch, err := storeA.GetPermissionEpoch(ctx, PermissionScopeGlobalOperator)
	if err != nil {
		t.Fatal(err)
	}
	if event := waitForEpoch(deletedEpoch); event.Epoch != deletedEpoch {
		t.Fatalf("delete notification = %+v, want epoch %d", event, deletedEpoch)
	}
	stopListener()
	select {
	case err := <-listenerErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("listener stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not stop")
	}
}

func TestPostgresV242ToV243MigrationIsIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, admin, quotedSchema := postgresTestSchema(t, ctx, dsn, "migration_v243")

	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()

	// Recreate the last released v2.4.2 boundary inside this isolated schema.
	// The next Open must add both chain-gap and permission repair objects.
	statements := []string{
		"DELETE FROM " + quotedSchema + ".schema_migrations WHERE version LIKE '2.4.3%'",
		"DELETE FROM " + quotedSchema + ".schema_migrations WHERE version LIKE '2.4.4%'",
		"DELETE FROM " + quotedSchema + ".schema_migrations WHERE version LIKE '2.4.5%'",
		"DELETE FROM " + quotedSchema + ".schema_migrations WHERE version='" + latestSchemaMigrationVersion + "'",
		"DROP INDEX IF EXISTS " + quotedSchema + ".idx_chain_watcher_gap_retention",
		"DROP INDEX IF EXISTS " + quotedSchema + ".idx_chain_watcher_gap_window_overlap",
		"DROP INDEX IF EXISTS " + quotedSchema + ".idx_chain_watcher_gap_fair_claim",
		"ALTER TABLE " + quotedSchema + ".chain_watcher_gap_tasks DROP COLUMN IF EXISTS head_event_id",
		"DROP TABLE IF EXISTS " + quotedSchema + ".broadcast_operator_permission_snapshots CASCADE",
		"DROP TABLE IF EXISTS " + quotedSchema + ".global_operator_level_repair_candidates CASCADE",
	}
	for _, statement := range statements {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare v2.4.2 schema: %v", err)
		}
	}

	assertMigrated := func(label string) {
		t.Helper()
		var migrationCount int
		if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+`.schema_migrations
			WHERE version IN ('2.4.3', '2.4.3-broadcast-permission-restore')`).Scan(&migrationCount); err != nil {
			t.Fatalf("%s migration markers: %v", label, err)
		}
		if migrationCount != 2 {
			t.Fatalf("%s migration markers = %d, want 2", label, migrationCount)
		}

		var headColumn, retentionIndex, overlapIndex, fairClaimIndex, snapshotsTable, candidatesTable bool
		if err := admin.QueryRow(ctx, `SELECT
			EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='chain_watcher_gap_tasks' AND column_name='head_event_id'),
			to_regclass($2) IS NOT NULL,
			to_regclass($3) IS NOT NULL,
			to_regclass($4) IS NOT NULL,
			to_regclass($5) IS NOT NULL,
			to_regclass($6) IS NOT NULL`,
			strings.Trim(quotedSchema, `"`),
			strings.Trim(quotedSchema, `"`)+".idx_chain_watcher_gap_retention",
			strings.Trim(quotedSchema, `"`)+".idx_chain_watcher_gap_window_overlap",
			strings.Trim(quotedSchema, `"`)+".idx_chain_watcher_gap_fair_claim",
			strings.Trim(quotedSchema, `"`)+".broadcast_operator_permission_snapshots",
			strings.Trim(quotedSchema, `"`)+".global_operator_level_repair_candidates",
		).Scan(&headColumn, &retentionIndex, &overlapIndex, &fairClaimIndex, &snapshotsTable, &candidatesTable); err != nil {
			t.Fatalf("%s schema objects: %v", label, err)
		}
		if !headColumn || !retentionIndex || !overlapIndex || !fairClaimIndex || !snapshotsTable || !candidatesTable {
			t.Fatalf("%s schema objects = head:%v retention:%v overlap:%v fair:%v snapshots:%v candidates:%v",
				label, headColumn, retentionIndex, overlapIndex, fairClaimIndex, snapshotsTable, candidatesTable)
		}
	}

	for attempt := 1; attempt <= 2; attempt++ {
		store, err = Open(ctx, migrationURL)
		if err != nil {
			t.Fatalf("Open after v2.4.2 attempt %d: %v", attempt, err)
		}
		store.Close()
		assertMigrated(fmt.Sprintf("attempt %d", attempt))
	}
}

func TestPostgresBroadcastGroupOwnershipMigrationUsesVerifiedCreatorEvidence(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_group_owner_migration")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	base := int64(940000000000 + now.UnixNano()%1000000)
	hostID := base
	defaultID := base + 1
	primaryID := base + 2
	otherPrimaryID := base + 3
	secondaryID := base + 4
	unknownID := base + 5
	chatOwned := -base
	chatOutOfScope := -base - 1
	for _, op := range []struct {
		userID, parentID, createdBy int64
		level                       string
	}{
		{primaryID, 0, hostID, "primary"},
		{otherPrimaryID, 0, hostID, "primary"},
		{secondaryID, primaryID, primaryID, "secondary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, op.createdBy, "migration fixture", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, chatID := range []int64{chatOwned, chatOutOfScope} {
		if err := store.EnsureGroup(ctx, chatID, fmt.Sprintf("group %d", chatID), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddBroadcastPermission(ctx, primaryID, "chat", chatOwned, "", hostID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, otherPrimaryID, "chat", chatOwned, "", hostID, now); err != nil {
		t.Fatal(err)
	}

	type historicalGroup struct {
		name      string
		createdBy int64
		chatID    int64
	}
	groups := []historicalGroup{
		{"owned-primary", primaryID, chatOwned},
		{"environment-host", hostID, chatOutOfScope},
		{"environment-default", defaultID, chatOutOfScope},
		{"secondary-ambiguous", secondaryID, chatOwned},
		{"unknown-ambiguous", unknownID, chatOwned},
		{"audit-conflict", primaryID, chatOwned},
		{"scope-conflict", primaryID, chatOutOfScope},
	}
	for _, group := range groups {
		if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_groups(name, created_by, created_at, updated_at)
			VALUES($1, $2, $3, $3)`, group.name, group.createdBy, now); err != nil {
			t.Fatalf("insert %s: %v", group.name, err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_group_chats(group_name, chat_id, created_at)
			VALUES($1, $2, $3)`, group.name, group.chatID, now); err != nil {
			t.Fatalf("add %s member: %v", group.name, err)
		}
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_group_audit_events(
		actor_user_id, action, group_name, created_at
	) VALUES($1, 'created', 'audit-conflict', $2)`, otherPrimaryID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM schema_migrations
		WHERE version LIKE '2.4.4%' OR version LIKE '2.4.5%' OR version=$1`, latestSchemaMigrationVersion); err != nil {
		t.Fatal(err)
	}
	store.Close()

	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, err := store.NormalizeBroadcastGroupOwnership(ctx, hostID, map[int64]struct{}{defaultID: {}}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.OwnedByPrimary != 1 || result.Environment != 2 || result.Ambiguous != 4 {
		t.Fatalf("ownership repair result = %+v", result)
	}

	wantOwners := map[string]int64{"owned-primary": primaryID}
	for _, group := range groups {
		got, ok, err := store.GetBroadcastGroup(ctx, group.name)
		if err != nil || !ok {
			t.Fatalf("get %s: ok=%v err=%v", group.name, ok, err)
		}
		if got.OwnerUserID != wantOwners[group.name] {
			t.Errorf("%s owner=%d want=%d", group.name, got.OwnerUserID, wantOwners[group.name])
		}
	}
	candidates, err := store.ListBroadcastGroupOwnerRepairCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]BroadcastGroupOwnerRepairCandidate, len(candidates))
	for _, candidate := range candidates {
		byName[candidate.GroupName] = candidate
	}
	for name, resolution := range map[string]string{
		"owned-primary":       "primary_owner",
		"environment-host":    "environment_owner",
		"environment-default": "environment_owner",
		"secondary-ambiguous": "ambiguous",
		"unknown-ambiguous":   "ambiguous",
		"audit-conflict":      "ambiguous",
		"scope-conflict":      "ambiguous",
	} {
		if byName[name].Resolution != resolution {
			t.Errorf("%s resolution=%q want=%q candidate=%+v", name, byName[name].Resolution, resolution, byName[name])
		}
	}
	if byName["scope-conflict"].OutOfScopeChatCount != 1 {
		t.Fatalf("scope conflict evidence = %+v", byName["scope-conflict"])
	}

	second, err := store.NormalizeBroadcastGroupOwnership(ctx, hostID, map[int64]struct{}{defaultID: {}}, now.Add(2*time.Second))
	if err != nil || second != (BroadcastGroupOwnerRepairResult{}) {
		t.Fatalf("second normalization = %+v err=%v", second, err)
	}
	renamed := "owned-primary-renamed"
	if ok, _, err := store.RenameBroadcastGroup(ctx, "owned-primary", renamed, primaryID, false, now.Add(3*time.Second)); err != nil || !ok {
		t.Fatalf("rename migrated owner group=%v err=%v", ok, err)
	}
	candidates, err = store.ListBroadcastGroupOwnerRepairCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundRenamedCandidate := false
	for _, candidate := range candidates {
		if candidate.GroupName == "owned-primary" {
			t.Fatal("owner repair candidate retained stale group name after rename")
		}
		if candidate.GroupName == renamed {
			foundRenamedCandidate = true
		}
	}
	if !foundRenamedCandidate {
		t.Fatal("renamed owner repair candidate was not preserved")
	}
	if _, err := store.pool.Exec(ctx, `UPDATE broadcast_group_audit_events SET action='mutated' WHERE group_name=$1`, renamed); err == nil {
		t.Fatal("broadcast group audit events should be immutable")
	}
}

func TestPostgresGlobalOperatorHierarchyRepair(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "operator_hierarchy_repair")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	base := int64(920000000000 + now.UnixNano()%1000000)
	hostID := base
	defaultID := base + 1
	primaryAID := base + 2
	primaryBID := base + 3
	secondaryAID := base + 4
	secondaryBID := base + 5
	explicitlyDisabledID := base + 6
	ambiguousID := base + 7
	validPrimaryID := base + 8
	hostCreatedSecondaryID := base + 9
	chatID := -base

	if _, err := store.pool.Exec(ctx, `DELETE FROM schema_migrations
		WHERE version LIKE '2.4.3%' OR version LIKE '2.4.4%' OR version LIKE '2.4.5%' OR version=$1`, latestSchemaMigrationVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `TRUNCATE global_operator_level_repair_candidates`); err != nil {
		t.Fatal(err)
	}
	insertOperator := func(userID int64, level, status string, parentID, createdBy, disabledBy int64) {
		t.Helper()
		var disabledAt any
		if status == "disabled" && disabledBy != 0 {
			disabledAt = now.Add(-time.Hour)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO global_operators(
			user_id, level, status, parent_user_id, created_by, created_at, disabled_by, disabled_at, remark
		) VALUES($1, $2, $3, NULLIF($4::BIGINT, 0::BIGINT), $5, $6,
			NULLIF($7::BIGINT, 0::BIGINT), $8, $9)`,
			userID, level, status, parentID, createdBy, now.Add(-2*time.Hour), disabledBy, disabledAt, fmt.Sprintf("operator-%d", userID)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_operators(
			user_id, status, created_by, remark, created_at, updated_at
		) VALUES($1, $2, $3, $4, $5, $5)`, userID, status, createdBy, fmt.Sprintf("operator-%d", userID), now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	insertOperator(hostID, "primary", "active", 0, 0, 0)
	insertOperator(defaultID, "primary", "active", 0, 0, 0)
	insertOperator(primaryAID, "secondary", "active", hostID, hostID, 0)
	insertOperator(primaryBID, "secondary", "active", hostID, hostID, 0)
	insertOperator(secondaryAID, "primary", "disabled", 0, primaryAID, 0)
	insertOperator(secondaryBID, "primary", "disabled", 0, primaryAID, 0)
	insertOperator(explicitlyDisabledID, "primary", "disabled", 0, primaryAID, primaryAID)
	insertOperator(ambiguousID, "primary", "active", 0, base+99, 0)
	insertOperator(validPrimaryID, "primary", "active", 0, hostID, 0)
	insertOperator(hostCreatedSecondaryID, "secondary", "active", validPrimaryID, hostID, 0)
	if _, err := store.pool.Exec(ctx, `INSERT INTO permission_audit_events(
		actor_user_id, subject_type, subject_user_id, action, level, created_at
	) VALUES($1, 'global_operator', $2, 'disabled', 'primary', $3)`, primaryAID, explicitlyDisabledID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatID, "legacy permission", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_operator_permissions(
		user_id, target, chat_id, group_name, granted_by, created_at
	) VALUES($1, 'chat', $2, '', $3, $4)`, secondaryBID, chatID, primaryAID, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	if err := store.migrate(ctx); err != nil {
		t.Fatalf("run v2.4.3 quarantine migration: %v", err)
	}
	var archivedPermissionCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_operator_permission_snapshots
		WHERE user_id=$1`, secondaryBID).Scan(&archivedPermissionCount); err != nil || archivedPermissionCount != 1 {
		t.Fatalf("quarantine snapshot count = %d, err=%v; want 1", archivedPermissionCount, err)
	}
	for _, userID := range []int64{hostID, defaultID, primaryAID, primaryBID, secondaryAID, secondaryBID, explicitlyDisabledID, ambiguousID, validPrimaryID, hostCreatedSecondaryID} {
		if active, err := store.IsGlobalOperator(ctx, userID); err != nil || active {
			t.Fatalf("user %d should be fail-closed after quarantine: active=%v err=%v", userID, active, err)
		}
	}

	result, err := store.NormalizeGlobalOperatorHierarchy(ctx, hostID, map[int64]struct{}{defaultID: {}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.PrimaryNormalized != 3 || result.SecondaryNormalized != 4 || result.Recovered != 2 || result.EnvDetached != 2 || result.Quarantined != 1 {
		t.Fatalf("repair result = %+v", result)
	}
	assertOperator := func(userID int64, level, status string, parentID int64) {
		t.Helper()
		op, ok, err := store.GetGlobalOperator(ctx, userID)
		if err != nil || !ok || op.Level != level || op.Status != status || op.ParentUserID != parentID {
			t.Fatalf("operator %d = %+v, ok=%v err=%v; want level=%s status=%s parent=%d", userID, op, ok, err, level, status, parentID)
		}
	}
	assertOperator(hostID, "primary", "disabled", 0)
	assertOperator(defaultID, "primary", "disabled", 0)
	assertOperator(primaryAID, "primary", "active", 0)
	assertOperator(primaryBID, "primary", "active", 0)
	assertOperator(secondaryAID, "secondary", "active", primaryAID)
	assertOperator(secondaryBID, "secondary", "active", primaryAID)
	assertOperator(explicitlyDisabledID, "secondary", "disabled", primaryAID)
	assertOperator(ambiguousID, "primary", "disabled", 0)
	assertOperator(validPrimaryID, "primary", "active", 0)
	assertOperator(hostCreatedSecondaryID, "secondary", "active", validPrimaryID)
	if allowed, err := store.HasBroadcastPermissionScope(ctx, secondaryBID, "chat", chatID, ""); err != nil || !allowed {
		t.Fatalf("recovered active identity lost old permission: allowed=%v err=%v", allowed, err)
	}
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_operator_permission_snapshots
		WHERE user_id=$1`, secondaryBID).Scan(&archivedPermissionCount); err != nil || archivedPermissionCount != 0 {
		t.Fatalf("recovered identity retained consumed snapshot: count=%d err=%v", archivedPermissionCount, err)
	}
	if again, err := store.NormalizeGlobalOperatorHierarchy(ctx, hostID, map[int64]struct{}{defaultID: {}}, now.Add(time.Minute)); err != nil || again.Changed() != 0 {
		t.Fatalf("second normalization = %+v, err=%v", again, err)
	}
	events, err := store.ListPermissionAuditEvents(ctx, secondaryAID, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundRecovered := false
	for _, event := range events {
		if event.Action == "hierarchy_recovered_secondary" && event.ParentUserID == primaryAID {
			foundRecovered = true
		}
	}
	if !foundRecovered {
		t.Fatalf("recovery audit missing: %+v", events)
	}
}

func TestPostgresOpenSkipsAppliedMigrationDuringBusinessWrite(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "migration_business_write")

	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("initial Open: %v", err)
	}
	defer store.Close()

	businessTx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin business write: %v", err)
	}
	defer businessTx.Rollback(context.Background())
	now := time.Now().UTC()
	userID := int64(890000000000 + now.UnixNano()%1000000)
	if _, err := businessTx.Exec(ctx, `INSERT INTO global_operators(
		user_id, level, status, parent_user_id, created_by, created_at, remark
	) VALUES($1, 'primary', 'active', NULL, $1, $2, 'migration concurrency')`, userID, now); err != nil {
		t.Fatalf("write global operator: %v", err)
	}
	if _, err := businessTx.Exec(ctx, `INSERT INTO broadcast_operators(
		user_id, status, created_by, remark, created_at, updated_at
	) VALUES($1, 'active', $1, 'migration concurrency', $2, $2)`, userID, now); err != nil {
		t.Fatalf("write broadcast operator: %v", err)
	}

	openCtx, openCancel := context.WithTimeout(ctx, 5*time.Second)
	defer openCancel()
	reopened, err := Open(openCtx, migrationURL)
	if err != nil {
		t.Fatalf("Open while business write holds table locks: %v", err)
	}
	reopened.Close()
}

func TestPostgresTelegramMetadataNewestUpdateWinsAcrossInstances(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	chatID := int64(-905000000000 - suffix%1000000)
	userID := int64(705000000000 + suffix%1000000)
	periodStart := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := storeA.EnsureGroup(ctx, chatID, "initial", periodStart); err != nil {
		t.Fatalf("ensure initial group: %v", err)
	}
	if err := storeA.SetGroupActivePeriod(ctx, chatID, true, "2026-07-15", "2026-07-15", periodStart); err != nil {
		t.Fatalf("start accounting period: %v", err)
	}

	releaseOlder := make(chan struct{})
	olderDone := make(chan error, 1)
	go func() {
		<-releaseOlder
		if err := storeB.EnsureGroupForTelegramUpdate(ctx, chatID, "older title", 100, periodStart.Add(time.Second)); err != nil {
			olderDone <- err
			return
		}
		olderDone <- storeB.TouchUserForTelegramUpdate(ctx, chatID, User{
			ID: userID, Username: "older", DisplayName: "Older Name",
		}, 100, periodStart.Add(time.Second))
	}()

	newerAt := periodStart.Add(2 * time.Second)
	if err := storeA.EnsureGroupForTelegramUpdate(ctx, chatID, "newest title", 200, newerAt); err != nil {
		t.Fatalf("write newer group metadata: %v", err)
	}
	if err := storeA.TouchUserForTelegramUpdate(ctx, chatID, User{
		ID: userID, Username: "newest", DisplayName: "Newest Name",
	}, 200, newerAt); err != nil {
		t.Fatalf("write newer user metadata: %v", err)
	}
	close(releaseOlder)
	if err := <-olderDone; err != nil {
		t.Fatalf("write delayed older metadata: %v", err)
	}
	if err := storeB.EnsureUser(ctx, chatID, User{
		ID: userID, Username: "historical", DisplayName: "Historical Reply Name",
	}, periodStart); err != nil {
		t.Fatalf("ensure historical reply user: %v", err)
	}

	group, err := storeB.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group after reverse completion: %v", err)
	}
	if group.Title != "newest title" || !group.Active || !group.ActivePeriodStartedAt.Equal(periodStart) {
		t.Fatalf("group metadata/state after reverse completion = %+v", group)
	}
	var groupUpdateID int64
	if err := storeB.pool.QueryRow(ctx, `SELECT last_telegram_update_id FROM groups WHERE chat_id=$1`, chatID).Scan(&groupUpdateID); err != nil {
		t.Fatalf("read group update id: %v", err)
	}
	var username, displayName string
	var userUpdateID int64
	var lastSeen time.Time
	if err := storeB.pool.QueryRow(ctx, `SELECT username,display_name,last_seen_at,last_telegram_update_id
		FROM users WHERE chat_id=$1 AND user_id=$2`, chatID, userID).Scan(
		&username, &displayName, &lastSeen, &userUpdateID,
	); err != nil {
		t.Fatalf("read user metadata: %v", err)
	}
	if groupUpdateID != 200 || username != "newest" || displayName != "Newest Name" || userUpdateID != 200 || !lastSeen.Equal(newerAt) {
		t.Fatalf("newest metadata did not win: group_update=%d user=%q/%q user_update=%d last_seen=%v",
			groupUpdateID, username, displayName, userUpdateID, lastSeen)
	}
}

func TestPostgresObservedMemberDoesNotEnterMentionAudienceUntilSpeaking(t *testing.T) {
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

	suffix := time.Now().UnixNano()
	chatID := int64(-906000000000 - suffix%1000000)
	userID := int64(706000000000 + suffix%1000000)
	now := time.Now().UTC().Truncate(time.Microsecond)
	member := User{ID: userID, Username: "member_only", DisplayName: "Member Only"}
	if err := store.ObserveUserForTelegramUpdate(ctx, chatID, member, 100, now); err != nil {
		t.Fatalf("observe member: %v", err)
	}
	if found, ok, err := store.FindUserByUsername(ctx, chatID, "member_only"); err != nil || !ok || found.ID != userID {
		t.Fatalf("find observed member = %+v, %v, %v", found, ok, err)
	}
	users, err := store.ListUsersForMention(ctx, chatID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("unspoken member entered notify-all audience: %+v", users)
	}
	if err := store.TouchUserForTelegramUpdate(ctx, chatID, member, 101, now.Add(time.Second)); err != nil {
		t.Fatalf("touch speaking member: %v", err)
	}
	users, err = store.ListUsersForMention(ctx, chatID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 || users[0].ID != userID {
		t.Fatalf("speaking member missing from notify-all audience: %+v", users)
	}
}

func TestPostgresBroadcastReplyPreferencesUpgradeAndOverrides(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, admin, quotedSchema := postgresTestSchema(t, ctx, dsn, "broadcast_reply_preferences")
	schemaName := strings.Trim(quotedSchema, `"`)

	fresh, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("fresh Open: %v", err)
	}
	fresh.Close()
	if _, err := admin.Exec(ctx, "DROP TABLE "+quotedSchema+".broadcast_reply_preferences"); err != nil {
		t.Fatalf("drop reply preference fixture: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE TABLE `+quotedSchema+`.broadcast_reply_preferences(
		observer_user_id BIGINT NOT NULL,source_user_id BIGINT NOT NULL,enabled BOOLEAN NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL,PRIMARY KEY(observer_user_id,source_user_id),
		CONSTRAINT broadcast_reply_preferences_distinct_users CHECK(observer_user_id<>source_user_id))`); err != nil {
		t.Fatalf("create legacy reply preference fixture: %v", err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+quotedSchema+`.broadcast_reply_preferences(observer_user_id,source_user_id,enabled,updated_at)
		VALUES(70001,70002,FALSE,NOW())`); err != nil {
		t.Fatalf("insert legacy reply preference fixture: %v", err)
	}
	if _, err := admin.Exec(ctx, "DELETE FROM "+quotedSchema+".schema_migrations WHERE version=$1", latestSchemaMigrationVersion); err != nil {
		t.Fatalf("delete reply preference marker: %v", err)
	}
	var memberMarker int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version=$1",
		chatMemberDiscoveryMigrationVersion).Scan(&memberMarker); err != nil || memberMarker != 1 {
		t.Fatalf("member marker count=%d err=%v", memberMarker, err)
	}

	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("upgrade Open: %v", err)
	}
	defer store.Close()
	var wg sync.WaitGroup
	openErrors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reopened, openErr := Open(ctx, migrationURL)
			if openErr == nil {
				reopened.Close()
			}
			openErrors <- openErr
		}()
	}
	wg.Wait()
	close(openErrors)
	for openErr := range openErrors {
		if openErr != nil {
			t.Fatalf("concurrent idempotent Open: %v", openErr)
		}
	}

	var tableCount, indexCount, constraintCount, markerCount int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_tables
		WHERE schemaname=$1 AND tablename='broadcast_reply_preferences'`, schemaName).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_indexes
		WHERE schemaname=$1 AND indexname='idx_broadcast_reply_preferences_source'`, schemaName).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_constraint c
		JOIN pg_catalog.pg_class r ON r.oid=c.conrelid
		JOIN pg_catalog.pg_namespace n ON n.oid=r.relnamespace
		WHERE n.nspname=$1 AND r.relname='broadcast_reply_preferences'
		  AND c.conname='broadcast_reply_preferences_distinct_users'`, schemaName).Scan(&constraintCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version=$1",
		latestSchemaMigrationVersion).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 1 || indexCount != 1 || constraintCount != 1 || markerCount != 1 {
		t.Fatalf("upgrade table=%d index=%d constraint=%d marker=%d", tableCount, indexCount, constraintCount, markerCount)
	}
	legacy, err := store.BroadcastMessagePreferenceOverrides(ctx, 70001)
	if err != nil || !legacy[70002].ReceiveBroadcast || legacy[70002].ReceiveReply {
		t.Fatalf("legacy preference migration=%+v err=%v", legacy, err)
	}

	observerA, observerB, source := int64(71001), int64(71002), int64(72001)
	overrides, err := store.BroadcastReplyPreferenceOverrides(ctx, observerA)
	if err != nil || len(overrides) != 0 {
		t.Fatalf("default overrides=%v err=%v", overrides, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.SetBroadcastReplyPreference(ctx, observerA, source, false, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetBroadcastReplyPreference(ctx, observerB, source, true, now); err != nil {
		t.Fatal(err)
	}
	bySource, err := store.BroadcastReplyPreferenceOverridesForSource(ctx, source, []int64{observerA, observerB, 79999})
	if err != nil || len(bySource) != 2 || bySource[observerA] || !bySource[observerB] {
		t.Fatalf("source overrides=%v err=%v", bySource, err)
	}
	if err := store.SetBroadcastReplyPreference(ctx, observerA, source, true, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	overrides, err = store.BroadcastReplyPreferenceOverrides(ctx, observerA)
	if err != nil || !overrides[source] {
		t.Fatalf("restored overrides=%v err=%v", overrides, err)
	}
	if err := store.SetBroadcastReplyPreference(ctx, source, source, false, now); err == nil {
		t.Fatal("self preference must be rejected")
	}
	if err := store.SetBroadcastMessagePreference(ctx, observerA, source, false, true, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	messagePreferences, err := store.BroadcastMessagePreferenceOverridesForSource(ctx, source, []int64{observerA, observerB})
	if err != nil || messagePreferences[observerA].ReceiveBroadcast || !messagePreferences[observerA].ReceiveReply || !messagePreferences[observerB].ReceiveBroadcast || !messagePreferences[observerB].ReceiveReply {
		t.Fatalf("message preferences=%+v err=%v", messagePreferences, err)
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
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, otherUserID, true, group, now.Add(10*time.Second)); err != nil || got.Status != LedgerClearTicketWrongUser {
		t.Fatalf("other user result = %+v, %v", got, err)
	}
	if record, ok, err := storeA.GetRecord(ctx, recordID); err != nil || !ok || record.DeletedAt != nil {
		t.Fatalf("other user changed record = %+v, ok=%t err=%v", record, ok, err)
	}
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, requesterID, false, group, now.Add(20*time.Second)); err != nil || got.Status != LedgerClearTicketPermissionDenied {
		t.Fatalf("revoked permission result = %+v, %v", got, err)
	}
	if record, ok, err := storeA.GetRecord(ctx, recordID); err != nil || !ok || record.DeletedAt != nil {
		t.Fatalf("revoked permission changed record = %+v, ok=%t err=%v", record, ok, err)
	}
	if got, err := storeB.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, requesterID, true, group, now.Add(59*time.Second)); err != nil || got.Status != LedgerClearTicketApplied || got.DeletedCount != 1 {
		t.Fatalf("second instance 59s result = %+v, %v", got, err)
	}
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, chatID, requesterID, true, group, now.Add(59*time.Second)); err != nil || got.Status != LedgerClearTicketConsumed {
		t.Fatalf("repeat result = %+v, %v", got, err)
	}

	expired := ticket
	expired.TokenHash = "clear-61-" + fmt.Sprint(suffix)
	if err := storeA.CreateLedgerClearTicket(ctx, expired); err != nil {
		t.Fatalf("create expiring ticket: %v", err)
	}
	if got, err := storeA.ConsumeLedgerClearTicketAndDelete(ctx, expired.TokenHash, chatID, requesterID, true, group, now.Add(61*time.Second)); err != nil || got.Status != LedgerClearTicketExpired {
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
	if got, err := storeB.ConsumeLedgerClearTicketAndDelete(ctx, oldPeriod.TokenHash, chatID, requesterID, true, group, now.Add(30*time.Second)); err != nil || got.Status != LedgerClearTicketNotFound {
		t.Fatalf("old period result = %+v, %v", got, err)
	}
}

func TestPostgresCutoffChangeInvalidatesLedgerClearTicketsAtomically(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	suffix := time.Now().UnixNano()
	requesterID := int64(730000000000 + suffix%1000000)

	setup := func(t *testing.T, chatID int64, cutoffHour int, token string) (Group, int64, LedgerClearTicket) {
		t.Helper()
		dayKey := "2026-07-15"
		periodStartedAt := now.Add(-time.Hour)
		if err := store.EnsureGroup(ctx, chatID, "cutoff clear ticket integration", periodStartedAt); err != nil {
			t.Fatalf("ensure group: %v", err)
		}
		if err := store.SetGroupCutoffState(ctx, chatID, cutoffHour, true, dayKey, dayKey, periodStartedAt); err != nil {
			t.Fatalf("set initial cutoff state: %v", err)
		}
		group, err := store.GetGroup(ctx, chatID)
		if err != nil {
			t.Fatalf("get initial group: %v", err)
		}
		recordID, err := store.InsertRecord(ctx, Record{
			ChatID: chatID, DayKey: dayKey, Kind: "deposit", Currency: "CNY", Amount: "1",
			Rate: "1", FeeRate: "0", ResultUSDT: "1", ActorUserID: requesterID,
			PeriodStartedAt: group.ActivePeriodStartedAt, CreatedAt: group.ActivePeriodStartedAt.Add(time.Second),
		})
		if err != nil {
			t.Fatalf("insert record: %v", err)
		}
		ticket := LedgerClearTicket{
			TokenHash: token, ChatID: chatID, RequestedByUserID: requesterID,
			DayKey: dayKey, ActivePeriodStartedAt: group.ActivePeriodStartedAt,
			ExpiresAt: now.Add(time.Minute), CreatedAt: now,
		}
		if err := store.CreateLedgerClearTicket(ctx, ticket); err != nil {
			t.Fatalf("create clear ticket: %v", err)
		}
		return group, recordID, ticket
	}

	assertTicketInvalidAndRecordIntact := func(t *testing.T, group Group, recordID int64, ticket LedgerClearTicket) {
		t.Helper()
		if _, ok, err := store.GetLedgerClearTicket(ctx, ticket.TokenHash); err != nil || ok {
			t.Fatalf("old clear ticket still exists: ok=%t err=%v", ok, err)
		}
		result, err := store.ConsumeLedgerClearTicketAndDelete(ctx, ticket.TokenHash, group.ChatID, requesterID, true, group, now.Add(10*time.Second))
		if err != nil || result.Status != LedgerClearTicketNotFound {
			t.Fatalf("consume invalidated ticket = %+v, %v", result, err)
		}
		record, ok, err := store.GetRecord(ctx, recordID)
		if err != nil || !ok || record.DeletedAt != nil {
			t.Fatalf("invalidated ticket changed record = %+v, ok=%t err=%v", record, ok, err)
		}
	}

	t.Run("zero to four keeps period and invalidates old ticket", func(t *testing.T) {
		chatID := int64(-930000000000 - suffix%1000000)
		before, recordID, ticket := setup(t, chatID, 0, fmt.Sprintf("cutoff-0-4-%d", suffix))
		if err := store.SetGroupCutoffState(ctx, chatID, 4, true, before.ActiveDayKey, before.ActiveExpiresDayKey, now); err != nil {
			t.Fatalf("set cutoff 4: %v", err)
		}
		after, err := store.GetGroup(ctx, chatID)
		if err != nil {
			t.Fatalf("get updated group: %v", err)
		}
		if !after.Active || after.CutoffHour != 4 || after.ActiveDayKey != before.ActiveDayKey || !after.ActivePeriodStartedAt.Equal(before.ActivePeriodStartedAt) {
			t.Fatalf("cutoff change altered active period: before=%+v after=%+v", before, after)
		}
		assertTicketInvalidAndRecordIntact(t, after, recordID, ticket)

		fresh := ticket
		fresh.TokenHash = fmt.Sprintf("cutoff-0-4-fresh-%d", suffix)
		fresh.ActivePeriodStartedAt = after.ActivePeriodStartedAt
		fresh.DayKey = after.ActiveDayKey
		if err := store.CreateLedgerClearTicket(ctx, fresh); err != nil {
			t.Fatalf("create fresh clear ticket: %v", err)
		}
		result, err := store.ConsumeLedgerClearTicketAndDelete(ctx, fresh.TokenHash, chatID, requesterID, true, after, now.Add(20*time.Second))
		if err != nil || result.Status != LedgerClearTicketApplied || result.DeletedCount != 1 {
			t.Fatalf("consume fresh ticket = %+v, %v", result, err)
		}
	})

	t.Run("four to disabled keeps period and invalidates old ticket", func(t *testing.T) {
		chatID := int64(-931000000000 - suffix%1000000)
		before, recordID, ticket := setup(t, chatID, 4, fmt.Sprintf("cutoff-4-disabled-%d", suffix))
		if err := store.SetGroupCutoffState(ctx, chatID, -1, true, before.ActiveDayKey, "", now); err != nil {
			t.Fatalf("disable cutoff: %v", err)
		}
		after, err := store.GetGroup(ctx, chatID)
		if err != nil {
			t.Fatalf("get updated group: %v", err)
		}
		if !after.Active || after.CutoffHour != -1 || after.ActiveDayKey != before.ActiveDayKey || after.ActiveExpiresDayKey != "" || !after.ActivePeriodStartedAt.Equal(before.ActivePeriodStartedAt) {
			t.Fatalf("disabling cutoff altered active period: before=%+v after=%+v", before, after)
		}
		assertTicketInvalidAndRecordIntact(t, after, recordID, ticket)
	})

	t.Run("ticket delete failure rolls back cutoff update", func(t *testing.T) {
		chatID := int64(-932000000000 - suffix%1000000)
		before, _, ticket := setup(t, chatID, 0, fmt.Sprintf("cutoff-rollback-%d", suffix))
		nameSuffix := fmt.Sprintf("%d", suffix)
		functionName := "test_fail_clear_ticket_delete_" + nameSuffix
		triggerName := "test_fail_clear_ticket_delete_trigger_" + nameSuffix
		quotedFunction := pgx.Identifier{functionName}.Sanitize()
		quotedTrigger := pgx.Identifier{triggerName}.Sanitize()
		if _, err := store.pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF OLD.chat_id = %d THEN
		RAISE EXCEPTION 'forced clear ticket delete failure';
	END IF;
	RETURN OLD;
END
$$`, quotedFunction, chatID)); err != nil {
			t.Fatalf("create failure function: %v", err)
		}
		if _, err := store.pool.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE DELETE ON ledger_clear_tickets
FOR EACH ROW EXECUTE FUNCTION %s()`, quotedTrigger, quotedFunction)); err != nil {
			_, _ = store.pool.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+quotedFunction+"()")
			t.Fatalf("create failure trigger: %v", err)
		}
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cleanupCancel()
			_, _ = store.pool.Exec(cleanupCtx, "DROP TRIGGER IF EXISTS "+quotedTrigger+" ON ledger_clear_tickets")
			_, _ = store.pool.Exec(cleanupCtx, "DROP FUNCTION IF EXISTS "+quotedFunction+"()")
		})

		if err := store.SetGroupCutoffState(ctx, chatID, 4, true, before.ActiveDayKey, before.ActiveExpiresDayKey, now); err == nil {
			t.Fatal("cutoff update succeeded despite forced ticket delete failure")
		}
		after, err := store.GetGroup(ctx, chatID)
		if err != nil {
			t.Fatalf("get group after rollback: %v", err)
		}
		if after.CutoffHour != before.CutoffHour || after.Active != before.Active || after.ActiveDayKey != before.ActiveDayKey || !after.ActivePeriodStartedAt.Equal(before.ActivePeriodStartedAt) {
			t.Fatalf("failed cutoff update was not rolled back: before=%+v after=%+v", before, after)
		}
		if _, ok, err := store.GetLedgerClearTicket(ctx, ticket.TokenHash); err != nil || !ok {
			t.Fatalf("ticket delete was not rolled back: ok=%t err=%v", ok, err)
		}
	})
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
	if renamed, affected, err := store.RenameBroadcastGroup(ctx, oldName, newName, userID+100, true, now.Add(time.Second)); err != nil || !renamed || len(affected) != 1 || affected[0] != userID {
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
	if deleted, affected, err := store.DeleteBroadcastGroupManaged(ctx, newName, userID+100, true, now.Add(2*time.Second)); err != nil || !deleted || len(affected) != 1 || affected[0] != userID {
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

func TestPostgresBroadcastGroupOwnershipAndDelegationBoundaries(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_group_scope")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	base := int64(950000000000 + now.UnixNano()%1000000)
	hostID := base
	primaryAID := base + 1
	primaryBID := base + 2
	secondaryAID := base + 3
	secondaryBID := base + 4
	chatAID := -base
	chatBID := -base - 1
	for _, op := range []struct {
		userID, parentID, createdBy int64
		level                       string
	}{
		{primaryAID, 0, hostID, "primary"},
		{primaryBID, 0, hostID, "primary"},
		{secondaryAID, primaryAID, primaryAID, "secondary"},
		{secondaryBID, primaryBID, primaryBID, "secondary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, op.createdBy, "scope fixture", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, chatID := range []int64{chatAID, chatBID} {
		if err := store.EnsureGroup(ctx, chatID, fmt.Sprintf("scope %d", chatID), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddBroadcastPermission(ctx, primaryAID, "chat", chatAID, "", hostID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryBID, "chat", chatBID, "", hostID, now); err != nil {
		t.Fatal(err)
	}
	groupName := fmt.Sprintf("owned-%d", base)
	created, err := store.CreateBroadcastGroup(ctx, groupName, primaryAID, primaryAID, now)
	if err != nil || !created {
		t.Fatalf("create owned group=%v err=%v", created, err)
	}
	if added, err := store.AddChatsToBroadcastGroupManaged(ctx, groupName, []int64{chatAID}, primaryAID, false, now); err != nil || added != 1 {
		t.Fatalf("owner add authorized chat=%d err=%v", added, err)
	}
	if _, err := store.AddChatsToBroadcastGroupManaged(ctx, groupName, []int64{chatBID}, primaryAID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("owner added out-of-scope chat: %v", err)
	}
	if _, _, err := store.RenameBroadcastGroup(ctx, groupName, groupName+"-forged", primaryBID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("peer renamed foreign group: %v", err)
	}
	if _, err := store.RemoveChatsFromBroadcastGroupManaged(ctx, groupName, []int64{chatAID}, primaryBID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("peer removed foreign group member: %v", err)
	}

	for _, subjectID := range []int64{secondaryAID, primaryBID} {
		if changed, err := store.GrantBroadcastPermissionAuthorized(ctx, subjectID, "chat", chatAID, "", primaryAID, false, now); err != nil || !changed {
			t.Fatalf("grant chat to %d changed=%v err=%v", subjectID, changed, err)
		}
		if changed, err := store.GrantBroadcastPermissionAuthorized(ctx, subjectID, "group", 0, groupName, primaryAID, false, now); err != nil || !changed {
			t.Fatalf("grant group to %d changed=%v err=%v", subjectID, changed, err)
		}
	}
	if _, err := store.GrantBroadcastPermissionAuthorized(ctx, secondaryBID, "chat", chatAID, "", primaryAID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("cross-parent secondary grant: %v", err)
	}
	if _, err := store.GrantBroadcastPermissionAuthorized(ctx, secondaryAID, "chat", chatBID, "", primaryAID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("out-of-scope direct chat grant: %v", err)
	}
	if _, err := store.GrantBroadcastPermissionAuthorized(ctx, primaryBID, "group", 0, groupName, secondaryAID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("secondary delegated group permission: %v", err)
	}
	invalidOwnerGroup := fmt.Sprintf("invalid-owner-%d", base)
	if err := store.UpsertBroadcastGroup(ctx, invalidOwnerGroup, hostID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE broadcast_groups SET owner_user_id=$2 WHERE name=$1`, invalidOwnerGroup, secondaryAID); err == nil {
		t.Fatal("database accepted a secondary as broadcast group owner")
	}

	if allowed, err := store.HasBroadcastGroupUse(ctx, primaryBID, groupName); err != nil || !allowed {
		t.Fatalf("peer primary group use=%v err=%v", allowed, err)
	}
	if _, _, err := store.DeleteBroadcastGroupManaged(ctx, groupName, primaryBID, false, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("group use granted management rights: %v", err)
	}
	visible, err := store.ListVisibleBroadcastGroups(ctx, primaryBID)
	if err != nil || len(visible) != 1 || visible[0].Name != groupName || visible[0].OwnerUserID != primaryAID {
		t.Fatalf("peer visible groups=%+v err=%v", visible, err)
	}

	if err := store.AddBroadcastPermission(ctx, secondaryAID, "chat", chatBID, "", hostID, now); err != nil {
		t.Fatal(err)
	}
	if result, err := store.RevokeBroadcastPermissionAuthorized(ctx, secondaryAID, "chat", chatBID, "", primaryAID, false, now); err != nil || result.Changed {
		t.Fatalf("primary revoked host grant: result=%+v err=%v", result, err)
	}
	if result, err := store.RevokeBroadcastPermissionAuthorized(ctx, primaryBID, "group", 0, groupName, primaryAID, false, now); err != nil || !result.Changed {
		t.Fatalf("primary revoke own peer grant: result=%+v err=%v", result, err)
	}
	if allowed, err := store.HasBroadcastGroupUse(ctx, primaryBID, groupName); err != nil || allowed {
		t.Fatalf("revoked peer still uses group=%v err=%v", allowed, err)
	}

	events, err := store.ListBroadcastGroupAuditEvents(ctx, groupName)
	if err != nil || len(events) < 2 {
		t.Fatalf("group audit events=%+v err=%v", events, err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM broadcast_group_audit_events WHERE group_name=$1`, groupName); err == nil {
		t.Fatal("broadcast group audit delete should be rejected")
	}
	if _, err := store.DisableGlobalOperator(ctx, primaryAID, hostID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	renamedGroup := groupName + "-disabled-owner"
	if renamed, _, err := store.RenameBroadcastGroup(ctx, groupName, renamedGroup, hostID, true, now.Add(2*time.Second)); err != nil || !renamed {
		t.Fatalf("host rename disabled owner's group=%v err=%v", renamed, err)
	}
	if group, ok, err := store.GetBroadcastGroup(ctx, renamedGroup); err != nil || !ok || group.OwnerUserID != primaryAID {
		t.Fatalf("renamed disabled-owner group=%+v ok=%v err=%v", group, ok, err)
	}
	if _, err := store.CreateBroadcastGroup(ctx, fmt.Sprintf("disabled-owner-%d", base), primaryAID, primaryAID, now.Add(3*time.Second)); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("disabled primary created owned group: %v", err)
	}
}

func TestPostgresBroadcastGroupOwnerTransferMigrationFromV244IsIdempotent(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	migrationURL, admin, quotedSchema := postgresTestSchema(t, ctx, dsn, "broadcast_owner_transfer_migration")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := admin.Exec(ctx, "DELETE FROM "+quotedSchema+".schema_migrations WHERE version IN ('2.4.5-broadcast-group-owner-transfer','"+latestSchemaMigrationVersion+"')"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "DROP TABLE "+quotedSchema+".broadcast_group_owner_transfer_events CASCADE"); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		store, err = Open(ctx, migrationURL)
		if err != nil {
			t.Fatalf("open migration attempt %d: %v", attempt, err)
		}
		store.Close()
		var markerCount int
		var tableExists bool
		if err := admin.QueryRow(ctx, "SELECT COUNT(*) FROM "+quotedSchema+`.schema_migrations
			WHERE version='2.4.5-broadcast-group-owner-transfer'`).Scan(&markerCount); err != nil {
			t.Fatal(err)
		}
		if err := admin.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`,
			strings.Trim(quotedSchema, `"`)+".broadcast_group_owner_transfer_events").Scan(&tableExists); err != nil {
			t.Fatal(err)
		}
		if markerCount != 1 || !tableExists {
			t.Fatalf("attempt %d marker=%d table=%v", attempt, markerCount, tableExists)
		}
	}
}

func TestPostgresBroadcastGroupOwnerTransferIsAtomicAuditedAndConcurrentSafe(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "broadcast_group_owner_transfer")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	base := int64(970000000000 + now.UnixNano()%1000000)
	hostID := base
	primaryAID := base + 1
	primaryBID := base + 2
	primaryCID := base + 3
	secondaryID := base + 4
	disabledPrimaryID := base + 5
	ordinaryID := base + 6
	chatAID := -base
	chatBID := -base - 1
	for _, op := range []struct {
		userID, parentID, createdBy int64
		level                       string
	}{
		{primaryAID, 0, hostID, "primary"},
		{primaryBID, 0, hostID, "primary"},
		{primaryCID, 0, hostID, "primary"},
		{secondaryID, primaryAID, primaryAID, "secondary"},
		{disabledPrimaryID, 0, hostID, "primary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, op.createdBy, "transfer fixture", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DisableGlobalOperator(ctx, disabledPrimaryID, hostID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchUser(ctx, primaryBID, User{ID: primaryBID, Username: "owner_b", DisplayName: "Owner B"}, now); err != nil {
		t.Fatal(err)
	}
	for _, chatID := range []int64{chatAID, chatBID} {
		if err := store.EnsureGroup(ctx, chatID, fmt.Sprintf("transfer %d", chatID), now); err != nil {
			t.Fatal(err)
		}
	}
	groupName := fmt.Sprintf("host-transfer-%d", base)
	if err := store.UpsertBroadcastGroup(ctx, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}
	if added, err := store.AddChatsToBroadcastGroupManaged(ctx, groupName, []int64{chatAID, chatBID}, hostID, true, now); err != nil || added != 2 {
		t.Fatalf("add host group chats=%d err=%v", added, err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryAID, "group", 0, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}

	if _, err := store.TransferBroadcastGroupOwner(ctx, groupName, 0, primaryAID, primaryAID, false, true, now); !errors.Is(err, ErrBroadcastScopeDenied) {
		t.Fatalf("non-host transfer error=%v", err)
	}
	for _, targetID := range []int64{secondaryID, disabledPrimaryID, ordinaryID} {
		if _, err := store.TransferBroadcastGroupOwner(ctx, groupName, 0, targetID, hostID, true, true, now); !errors.Is(err, ErrBroadcastGroupOwnerInvalid) {
			t.Fatalf("invalid owner %d error=%v", targetID, err)
		}
	}

	missing, err := store.TransferBroadcastGroupOwner(ctx, groupName, 0, primaryAID, hostID, true, false, now.Add(2*time.Second))
	if !errors.Is(err, ErrBroadcastGroupOwnerPermissionsMissing) || missing.MissingChatPermission != 2 {
		t.Fatalf("missing permission result=%+v err=%v", missing, err)
	}
	if group, ok, err := store.GetBroadcastGroup(ctx, groupName); err != nil || !ok || group.OwnerUserID != 0 {
		t.Fatalf("rejected transfer changed group=%+v ok=%v err=%v", group, ok, err)
	}
	var rejectedDirectGrantCount int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM broadcast_operator_permissions
		WHERE user_id=$1::BIGINT AND target='chat'`, primaryAID).Scan(&rejectedDirectGrantCount); err != nil || rejectedDirectGrantCount != 0 {
		t.Fatalf("rejected transfer direct grants=%d err=%v", rejectedDirectGrantCount, err)
	}

	first, err := store.TransferBroadcastGroupOwner(ctx, groupName, 0, primaryAID, hostID, true, true, now.Add(3*time.Second))
	if err != nil || !first.Changed || first.MissingChatPermission != 2 || first.AutoGrantedChatPermission != 2 {
		t.Fatalf("first transfer result=%+v err=%v", first, err)
	}
	var grantedCount int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM broadcast_operator_permissions
		WHERE user_id=$1::BIGINT AND target='chat' AND granted_by=$2::BIGINT`, primaryAID, hostID).Scan(&grantedCount); err != nil || grantedCount != 2 {
		t.Fatalf("auto grants=%d err=%v", grantedCount, err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, primaryAID, "group", 0, groupName); err != nil || !allowed {
		t.Fatalf("existing group permission lost allowed=%v err=%v", allowed, err)
	}
	for _, targetID := range []int64{primaryBID, primaryCID} {
		for _, chatID := range []int64{chatAID, chatBID} {
			if err := store.AddBroadcastPermission(ctx, targetID, "chat", chatID, "", hostID, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	second, err := store.TransferBroadcastGroupOwner(ctx, groupName, primaryAID, primaryBID, hostID, true, false, now.Add(4*time.Second))
	if err != nil || !second.Changed || second.MissingChatPermission != 0 || second.AutoGrantedChatPermission != 0 {
		t.Fatalf("zero-fill transfer result=%+v err=%v", second, err)
	}
	if group, ok, err := store.GetBroadcastGroup(ctx, groupName); err != nil || !ok || group.OwnerUserID != primaryBID ||
		group.CreatedBy != hostID || group.OwnerUsername != "owner_b" || group.OwnerStatus != "active" {
		t.Fatalf("new owner display fields group=%+v ok=%v err=%v", group, ok, err)
	}
	if _, err := store.TransferBroadcastGroupOwner(ctx, groupName, primaryAID, primaryBID, hostID, true, false, now.Add(5*time.Second)); !errors.Is(err, ErrBroadcastGroupOwnerChanged) {
		t.Fatalf("stale repeat error=%v", err)
	}
	if repeat, err := store.TransferBroadcastGroupOwner(ctx, groupName, primaryBID, primaryBID, hostID, true, false, now.Add(6*time.Second)); err != nil || repeat.Changed {
		t.Fatalf("same-owner repeat result=%+v err=%v", repeat, err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	for _, targetID := range []int64{primaryAID, primaryCID} {
		targetID := targetID
		go func() {
			<-start
			_, err := store.TransferBroadcastGroupOwner(ctx, groupName, primaryBID, targetID, hostID, true, false, now.Add(7*time.Second))
			results <- err
		}()
	}
	close(start)
	var successes, stale int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrBroadcastGroupOwnerChanged):
			stale++
		default:
			t.Fatalf("concurrent transfer error=%v", err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent results successes=%d stale=%d", successes, stale)
	}

	events, err := store.ListBroadcastGroupOwnerTransferEvents(ctx, groupName)
	if err != nil || len(events) != 3 {
		t.Fatalf("transfer events=%+v err=%v", events, err)
	}
	if events[0].PreviousOwnerUserID != 0 || events[0].NewOwnerUserID != primaryAID || events[0].AutoGrantedChatPermission != 2 {
		t.Fatalf("first transfer audit=%+v", events[0])
	}
	groupEvents, err := store.ListBroadcastGroupAuditEvents(ctx, groupName)
	if err != nil {
		t.Fatal(err)
	}
	ownerTransferAudits := 0
	for _, event := range groupEvents {
		if event.Action == "owner_transferred" {
			ownerTransferAudits++
		}
	}
	if ownerTransferAudits != 3 {
		t.Fatalf("generic owner transfer audits=%d events=%+v", ownerTransferAudits, groupEvents)
	}
	var permissionAuditCount int
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM permission_audit_events
		WHERE actor_user_id=$1::BIGINT AND subject_user_id=$2::BIGINT
		  AND subject_type='broadcast_permission' AND action='granted' AND target_type='chat'`, hostID, primaryAID).Scan(&permissionAuditCount); err != nil || permissionAuditCount != 2 {
		t.Fatalf("permission audit count=%d err=%v", permissionAuditCount, err)
	}
	if _, err := store.pool.Exec(ctx, `DELETE FROM broadcast_group_owner_transfer_events WHERE group_name=$1`, groupName); err == nil {
		t.Fatal("owner transfer audit delete should be rejected")
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
	if !group.ActivePeriodStartedAt.Equal(firstPeriodStart) {
		t.Fatalf("same-day restart changed period start: %v != %v", group.ActivePeriodStartedAt, firstPeriodStart)
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
	if err := store.CreateAdminLoginTicket(ctx, tokenHash, userID, now.Add(time.Minute), now); err != nil {
		t.Fatalf("create admin login ticket: %v", err)
	}
	ticket, ok, err := store.GetAdminLoginTicket(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("get admin login ticket: %v", err)
	}
	if !ok || ticket.UserID != userID || ticket.Role != "identity" {
		t.Fatalf("unexpected admin ticket: ok=%v ticket=%+v", ok, ticket)
	}
	ticket, ok, err = store.ConsumeAdminLoginTicket(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("consume admin login ticket: %v", err)
	}
	if !ok || ticket.UserID != userID || ticket.Role != "identity" {
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
	if err := store.CreateAdminLoginTicket(ctx, expiredTokenHash, userID, now.Add(-time.Minute), now.Add(-2*time.Minute)); err != nil {
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
	if allowed, err := store.HasBroadcastPermissionScope(ctx, userID, "chat", chatID, ""); err != nil || !allowed {
		t.Fatalf("reenabled primary did not restore chat permission: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, secondaryUserID, "chat", chatID, ""); err != nil || allowed {
		t.Fatalf("disabled secondary restored before reenable: allowed=%v err=%v", allowed, err)
	}
	if err := store.UpsertGlobalOperator(ctx, secondaryUserID, "secondary", userID, 9999, "reenabled secondary", now.Add(2*time.Second)); err != nil {
		t.Fatalf("reenable secondary: %v", err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, secondaryUserID, "chat", chatID, ""); err != nil || !allowed {
		t.Fatalf("reenabled secondary did not restore chat permission: allowed=%v err=%v", allowed, err)
	}
	if removed, err := store.RemoveBroadcastPermission(ctx, userID, "chat", chatID, "", 9999, now.Add(3*time.Second)); err != nil || !removed {
		t.Fatalf("remove restored permission: removed=%v err=%v", removed, err)
	}
	if disabled, err := store.DisableGlobalOperator(ctx, userID, 9999, now.Add(4*time.Second)); err != nil || !disabled {
		t.Fatalf("disable after explicit revoke: disabled=%v err=%v", disabled, err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO broadcast_operator_permission_snapshots(
		user_id, target, chat_id, group_name, granted_by, original_created_at, archived_by, archived_at
	) VALUES($1, 'group', 0, 'deleted-group', $2, $3, $2, $3)`, userID, int64(9999), now.Add(4*time.Second)); err != nil {
		t.Fatalf("insert stale permission snapshot: %v", err)
	}
	if err := store.UpsertGlobalOperator(ctx, userID, "primary", 0, 9999, "reenabled without revoked scope", now.Add(5*time.Second)); err != nil {
		t.Fatalf("reenable after explicit revoke: %v", err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, userID, "chat", chatID, ""); err != nil || allowed {
		t.Fatalf("explicitly revoked permission was resurrected: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, userID, "group", 0, "deleted-group"); err != nil || allowed {
		t.Fatalf("deleted group snapshot was restored: allowed=%v err=%v", allowed, err)
	}
	var archivedPermissionCount int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM broadcast_operator_permission_snapshots
		WHERE user_id=$1`, userID).Scan(&archivedPermissionCount); err != nil || archivedPermissionCount != 0 {
		t.Fatalf("stale snapshot was not consumed: count=%d err=%v", archivedPermissionCount, err)
	}
	auditEvents, err := store.ListPermissionAuditEvents(ctx, userID, 20)
	if err != nil {
		t.Fatalf("list permission audit: %v", err)
	}
	actions := map[string]bool{}
	for _, event := range auditEvents {
		actions[event.Action] = true
	}
	for _, action := range []string{"created", "disabled", "reenabled", "restored"} {
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
	lateDelivery := delivery
	lateDelivery.DeliveryID += "-second-subscriber"
	lateDelivery.OwnerUserID = 2
	lateDelivery.ChatID = 2
	inserted, err := store.RecordChainWatcherMatches(ctx, event, []ChainWatcherMatchedEvent{lateDelivery}, time.Now())
	if err != nil || inserted != 1 {
		t.Fatalf("existing event new subscriber delivery = %d/%v, want 1/nil", inserted, err)
	}

	second := event
	second.EventID += "-log1"
	second.EventIndex = "1"
	secondDelivery := delivery
	secondDelivery.EventID, secondDelivery.DeliveryID = second.EventID, delivery.DeliveryID+"-log1"
	inserted, err = store.RecordChainWatcherMatches(ctx, second, []ChainWatcherMatchedEvent{secondDelivery}, time.Now())
	if err != nil || inserted != 1 {
		t.Fatalf("second log event = %d/%v", inserted, err)
	}
}

func TestChainNotificationOutboxIsAtomicCriticalAndEventDeduplicated(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "chain_outbox_critical")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	suffix := fmt.Sprint(now.UnixNano())
	inserted, err := store.RecordChainNotificationOutboxEvent(ctx, 1001, "TAddress", "tx-"+suffix, "event-"+suffix, "income", now.UnixMilli(), 1001, "notice", "HTML", true, now)
	if err != nil || !inserted {
		t.Fatalf("first chain outbox insert = %v/%v", inserted, err)
	}
	inserted, err = store.RecordChainNotificationOutboxEvent(ctx, 1001, "TAddress", "tx-"+suffix, "event-"+suffix, "income", now.UnixMilli(), 1001, "duplicate", "HTML", true, now.Add(time.Second))
	if err != nil || inserted {
		t.Fatalf("duplicate chain outbox insert = %v/%v", inserted, err)
	}
	var count int
	var priority int
	var status, text string
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*),MIN(priority),MIN(status),MIN(text)
		FROM notification_outbox WHERE kind='chain' AND dedupe_key=$1`,
		"chain:1001:TAddress:event-"+suffix+":income").Scan(&count, &priority, &status, &text); err != nil {
		t.Fatal(err)
	}
	if count != 1 || priority != 0 || status != "pending" || text != "notice" {
		t.Fatalf("chain outbox row = count %d priority %d status %s text %s", count, priority, status, text)
	}
}

func TestChainWatcherPriorityPersistenceUsesReservedPool(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "watcher_priority_pool")
	store, err := OpenChainWatcher(ctx, migrationURL, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	held := make([]*pgxpool.Conn, 0, 32)
	for index := 0; index < 32; index++ {
		conn, acquireErr := store.pool.Acquire(ctx)
		if acquireErr != nil {
			t.Fatalf("acquire normal connection %d: %v", index, acquireErr)
		}
		held = append(held, conn)
	}
	defer func() {
		for _, conn := range held {
			conn.Release()
		}
	}()

	suffix := fmt.Sprint(time.Now().UnixNano())
	event := ChainWatcherEvent{EventID: "priority-event-" + suffix, TxHash: "priority-tx-" + suffix, From: "A", To: "B", Value: "1", Source: "realtime"}
	delivery := ChainWatcherMatchedEvent{DeliveryID: "priority-delivery-" + suffix, EventID: event.EventID, BotID: "bot", ChatID: 1, OwnerUserID: 1, WatchAddress: "B", Direction: "income"}
	priorityCtx, priorityCancel := context.WithTimeout(context.Background(), time.Second)
	defer priorityCancel()
	inserted, err := store.RecordChainWatcherMatchesPriority(priorityCtx, event, []ChainWatcherMatchedEvent{delivery}, time.Now())
	if err != nil || inserted != 1 {
		t.Fatalf("priority persistence while normal pool saturated = %d/%v", inserted, err)
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

	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "dual_anchor_restart")
	first, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC().UnixMilli()
	continuityID := fmt.Sprintf("restart-continuity-%d", timestamp)
	headID := fmt.Sprintf("restart-head-%d", timestamp)
	if err := first.AdvanceChainWatcherWatermark(ctx, timestamp-2000, continuityID, "catchup", time.Now().UTC()); err != nil {
		first.Close()
		t.Fatal(err)
	}
	if advanced, err := first.AdvanceChainWatcherRealtimeWatermark(ctx, timestamp, headID, time.Now().UTC()); err != nil || !advanced {
		first.Close()
		t.Fatalf("advance realtime head = %v/%v", advanced, err)
	}
	failedCtx, failedCancel := context.WithCancel(context.Background())
	failedCancel()
	if advanced, err := first.AdvanceChainWatcherRealtimeWatermark(failedCtx, timestamp+1000, "must-not-commit", time.Now().UTC()); err == nil || advanced {
		first.Close()
		t.Fatalf("cancelled realtime head write = %v/%v, want false/error", advanced, err)
	}
	first.Close()

	second, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	watermark, err := second.GetChainWatcherWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if watermark.Timestamp != timestamp-2000 || watermark.TxHash != continuityID {
		t.Fatalf("continuity watermark after reopen = %+v, want %d/%s", watermark, timestamp-2000, continuityID)
	}
	realtime, err := second.GetChainWatcherRealtimeWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if realtime.Timestamp != timestamp || realtime.TxHash != headID {
		t.Fatalf("head anchor after reopen = %+v, want %d/%s", realtime, timestamp, headID)
	}
}

func TestChainWatcherGapDiagnosticsWindowsAndRetention(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_metrics_retention")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Minute).Add(30 * time.Second)
	for index, age := range []time.Duration{30 * time.Second, 3 * time.Minute, 30 * time.Minute} {
		createdAt := now.Add(-age)
		if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
			Kind: "page", Source: "metrics", Priority: 2, Reason: "window-test",
			FromTimestamp: createdAt.UnixMilli(), ToTimestamp: createdAt.Add(time.Second).UnixMilli(),
			StartPage: index, EndPage: index + 1, NextPage: index,
		}, createdAt); err != nil {
			t.Fatal(err)
		}
	}
	diagnostics, err := store.ChainWatcherGapDiagnostics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]int64{1: 1, 5: 2, 60: 3}
	for window, expected := range want {
		var got int64
		for _, metric := range diagnostics.Metrics {
			if metric.WindowMinutes == window && metric.Kind == "page" && metric.Priority == 2 {
				got = metric.CreatedCount
			}
		}
		if got != expected {
			t.Fatalf("%d-minute created count = %d, want %d; metrics=%+v", window, got, expected, diagnostics.Metrics)
		}
	}

	old := now.Add(-73 * time.Hour)
	if _, err := store.pool.Exec(ctx, `INSERT INTO chain_watcher_gap_metric_minutes(
		bucket_at,kind,priority,created_count,completed_count,merged_count,failed_count,fairness_selected_count
	) VALUES($1,'old',9,1,0,0,0,0)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `INSERT INTO chain_watcher_gap_tasks(
		kind,source,priority,reason,from_timestamp,to_timestamp,start_page,end_page,next_page,status,created_at,updated_at,completed_at
	) VALUES('page','retention',9,'old',1,2,0,1,0,'completed',$1,$1,$1)`, old); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CleanupChainWatcherRetention(ctx, 10*time.Minute, now); err != nil {
		t.Fatal(err)
	}
	var oldMetrics, oldCompleted int
	if err := store.pool.QueryRow(ctx, `SELECT
		(SELECT COUNT(*) FROM chain_watcher_gap_metric_minutes WHERE kind='old'),
		(SELECT COUNT(*) FROM chain_watcher_gap_tasks WHERE source='retention')`).Scan(&oldMetrics, &oldCompleted); err != nil {
		t.Fatal(err)
	}
	if oldMetrics != 0 || oldCompleted != 0 {
		t.Fatalf("72h cleanup left old metrics/tasks = %d/%d", oldMetrics, oldCompleted)
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

func TestChainWatcherGapClaimAgesOldLowerPriorityWork(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_aging_fairness")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := now.UnixMilli()
	oldCreatedAt := now.Add(-15 * time.Minute)
	oldID, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "window", Source: "watcher", Priority: 2, Reason: "old-window",
		FromTimestamp: base - 60_000, ToTimestamp: base - 30_000,
	}, oldCreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "expand", Source: "watcher", Priority: 1, Reason: "new-expand",
		FromTimestamp: base - 10_000, ToTimestamp: base,
		StartPage: 3, EndPage: 20, NextPage: 3, AnchorEventID: "new-anchor",
	}, now); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := store.ClaimChainWatcherGap(ctx, "fair-worker", "watcher_fair", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("claim = %+v/%v/%v", claimed, ok, err)
	}
	if claimed.ID != oldID || claimed.Reason != "old-window" {
		t.Fatalf("claimed %+v, want aged lower-priority gap %d", claimed, oldID)
	}
}

func TestChainWatcherOverlappingWindowsCoalesceConcurrentlyAndSurviveRestart(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_coalesce")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	base := now.UnixMilli()
	const writers = 32
	start := make(chan struct{})
	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		writer := writer
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, enqueueErr := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
				Kind: "window", Source: "watcher", Priority: 10, Reason: "overlap",
				FromTimestamp: base + int64(writer), ToTimestamp: base + 10_000 + int64(writer),
			}, now.Add(time.Duration(writer)*time.Millisecond))
			errCh <- enqueueErr
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for enqueueErr := range errCh {
		if enqueueErr != nil {
			t.Fatalf("concurrent enqueue: %v", enqueueErr)
		}
	}
	stats, err := store.ChainWatcherGapStats(ctx, now.Add(time.Second))
	if err != nil || stats.PendingCount != 1 || stats.LeasedCount != 0 {
		t.Fatalf("coalesced stats = %+v/%v, want one pending", stats, err)
	}
	store.Close()

	reopened, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	task, ok, err := reopened.ClaimChainWatcherGap(ctx, "restart-worker", "watcher", time.Second, now.Add(2*time.Second))
	if err != nil || !ok {
		t.Fatalf("claim after restart = %+v/%v/%v", task, ok, err)
	}
	if task.FromTimestamp != base || task.ToTimestamp != base+10_000+writers-1 {
		t.Fatalf("coalesced range = %d..%d", task.FromTimestamp, task.ToTimestamp)
	}
	if completed, err := reopened.CompleteChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, now.Add(2*time.Second)); err != nil || !completed {
		t.Fatalf("complete coalesced task = %v/%v", completed, err)
	}
	stats, err = reopened.ChainWatcherGapStats(ctx, now.Add(3*time.Second))
	if err != nil || stats.PendingCount != 0 || stats.LeasedCount != 0 {
		t.Fatalf("drained stats = %+v/%v", stats, err)
	}
}

func TestChainWatcherWindowSplitRestartsBothChildPageCursors(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_split_cursor")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	base := now.UnixMilli()
	if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "window", Source: "watcher", Priority: 10,
		FromTimestamp: base, ToTimestamp: base + 10_000,
	}, now); err != nil {
		t.Fatal(err)
	}
	task, ok, err := store.ClaimChainWatcherGap(ctx, "split-worker", "watcher", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("claim = %+v/%v/%v", task, ok, err)
	}
	if advanced, err := store.AdvanceChainWatcherGapPage(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, 19, time.Minute, now); err != nil || !advanced {
		t.Fatalf("advance page = %v/%v", advanced, err)
	}
	if split, err := store.SplitChainWatcherGapWindow(ctx, task, base+5_000, now.Add(time.Second)); err != nil || !split {
		t.Fatalf("split = %v/%v", split, err)
	}
	for child := 0; child < 2; child++ {
		claimed, ok, err := store.ClaimChainWatcherGap(ctx, fmt.Sprintf("child-%d", child), "watcher", time.Minute, now.Add(2*time.Second))
		if err != nil || !ok {
			t.Fatalf("child %d claim = %+v/%v/%v", child, claimed, ok, err)
		}
		if claimed.NextPage != claimed.StartPage {
			t.Fatalf("child %d cursor = %d, want %d", child, claimed.NextPage, claimed.StartPage)
		}
		if completed, err := store.CompleteChainWatcherGap(ctx, claimed.ID, claimed.LeaseGeneration, claimed.LeaseOwner, now.Add(2*time.Second)); err != nil || !completed {
			t.Fatalf("child %d complete = %v/%v", child, completed, err)
		}
	}
}

func TestChainWatcherOrdinaryCursorUpdatesNeverRegress(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_cursor_monotonic")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	base := now.UnixMilli()
	if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "window", Source: "fallback", Priority: 1,
		FromTimestamp: base, ToTimestamp: base + 10_000,
	}, now); err != nil {
		t.Fatal(err)
	}
	task, ok, err := store.ClaimChainWatcherGap(ctx, "cursor-worker", "fallback", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("claim = %+v/%v/%v", task, ok, err)
	}
	if advanced, err := store.AdvanceChainWatcherGapPage(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, 53, time.Minute, now); err != nil || !advanced {
		t.Fatalf("advance to 53 = %v/%v", advanced, err)
	}
	if yielded, err := store.YieldChainWatcherGap(ctx, task.ID, task.LeaseGeneration, task.LeaseOwner, 36, "", now.Add(time.Second)); err != nil || !yielded {
		t.Fatalf("stale yield = %v/%v", yielded, err)
	}
	claimed, ok, err := store.ClaimChainWatcherGap(ctx, "cursor-worker-2", "fallback", time.Minute, now.Add(2*time.Second))
	if err != nil || !ok {
		t.Fatalf("reclaim = %+v/%v/%v", claimed, ok, err)
	}
	if claimed.NextPage != 53 {
		t.Fatalf("ordinary cursor regressed to %d, want 53", claimed.NextPage)
	}
}

func TestNormalizeChainWatcherGapBacklogCollapsesLegacyOverlap(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_legacy_normalize")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	base := now.UnixMilli()
	const legacyWindows = 800
	for index := 0; index < legacyWindows; index++ {
		from := base + int64(index)
		to := base + 10_000 + int64(index)
		if _, err := store.pool.Exec(ctx, `INSERT INTO chain_watcher_gap_tasks(
			kind,source,priority,reason,from_timestamp,to_timestamp,status,created_at,updated_at
		) VALUES('window','watcher',10,'legacy-overlap',$1,$2,'pending',$3,$3)`, from, to, now); err != nil {
			t.Fatal(err)
		}
	}
	before, err := store.ChainWatcherGapStats(ctx, now)
	if err != nil || before.PendingCount != legacyWindows {
		t.Fatalf("before normalize = %+v/%v", before, err)
	}
	started := time.Now()
	merged, err := store.NormalizeChainWatcherGapBacklog(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	after, err := store.ChainWatcherGapStats(ctx, now.Add(time.Second))
	if err != nil || merged != legacyWindows-1 || after.PendingCount != 1 {
		t.Fatalf("normalized merged/stats = %d/%+v/%v", merged, after, err)
	}
	t.Logf("normalized %d overlapping legacy windows to 1 in %s", legacyWindows, time.Since(started))
	task, ok, err := store.ClaimChainWatcherGap(ctx, "normalized-worker", "watcher", time.Minute, now.Add(2*time.Second))
	if err != nil || !ok || task.FromTimestamp != base || task.ToTimestamp != base+10_000+legacyWindows-1 {
		t.Fatalf("normalized task = %+v/%v/%v", task, ok, err)
	}
}

func TestOverlappingWindowGapsMergeAcrossTransientHeadLabels(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_head_identity")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := now.UnixMilli()
	for index, headEventID := range []string{"head-a", "head-b"} {
		if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
			Kind: "window", Source: "watcher", Priority: 10, Reason: "head-boundary",
			FromTimestamp: base + int64(index), ToTimestamp: base + 10_000 + int64(index),
			HeadEventID: headEventID,
		}, now.Add(time.Duration(index)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}

	merged, err := store.NormalizeChainWatcherGapBacklog(ctx, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := store.ChainWatcherGapStats(ctx, now.Add(time.Second))
	if err != nil || stats.PendingCount != 1 {
		t.Fatalf("overlapping head-labelled windows = %d/%+v/%v, want one pending", merged, stats, err)
	}
	task, ok, err := store.ClaimChainWatcherGap(ctx, "head-worker", "watcher", time.Minute, now.Add(2*time.Second))
	if err != nil || !ok || task.HeadEventID != "" {
		t.Fatalf("merged window task = %+v/%v/%v", task, ok, err)
	}
}

func TestLeasedAnchorGapKeepsAtMostOnePendingOverlapSuccessor(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_leased_successor")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	base := now.UnixMilli()
	if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "expand", Source: "watcher", Priority: 1, Reason: "first-anchor",
		FromTimestamp: base - 600_000, ToTimestamp: base,
		StartPage: 2, EndPage: 4096, NextPage: 2, AnchorEventID: "anchor-0",
	}, now); err != nil {
		t.Fatal(err)
	}
	leased, ok, err := store.ClaimChainWatcherGap(ctx, "leased-worker", "watcher_priority", time.Minute, now)
	if err != nil || !ok || leased.Kind != "expand" {
		t.Fatalf("leased anchor = %+v/%v/%v", leased, ok, err)
	}
	for round := 1; round <= 100; round++ {
		if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
			Kind: "expand", Source: "watcher", Priority: 1, Reason: "next-anchor",
			FromTimestamp: base - 600_000 + int64(round), ToTimestamp: base + int64(round),
			StartPage: 2, EndPage: 4096, NextPage: 2, AnchorEventID: fmt.Sprintf("anchor-%d", round),
		}, now.Add(time.Duration(round)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := store.ChainWatcherGapStats(ctx, now.Add(time.Second))
	if err != nil || stats.LeasedCount != 1 || stats.PendingCount != 1 {
		t.Fatalf("leased/pending overlap stats = %+v/%v, want 1/1", stats, err)
	}
}

func TestProgressedWindowGapKeepsCursorAndOneUnstartedSuccessor(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_progress_successor")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := now.UnixMilli()
	originalID, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
		Kind: "window", Source: "watcher", Priority: 1, Reason: "original",
		FromTimestamp: base, ToTimestamp: base + 10_000,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := store.ClaimChainWatcherGap(ctx, "progress-worker", "watcher_priority", time.Minute, now)
	if err != nil || !ok || claimed.ID != originalID {
		t.Fatalf("claim original = %+v/%v/%v", claimed, ok, err)
	}
	if yielded, err := store.YieldChainWatcherGap(ctx, claimed.ID, claimed.LeaseGeneration, claimed.LeaseOwner, 7, "", now.Add(time.Second)); err != nil || !yielded {
		t.Fatalf("yield original = %v/%v", yielded, err)
	}

	for round := 1; round <= 100; round++ {
		if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
			Kind: "window", Source: "watcher", Priority: 1, Reason: "growing-successor",
			FromTimestamp: base + int64(round), ToTimestamp: base + 10_000 + int64(round),
		}, now.Add(time.Second+time.Duration(round)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.NormalizeChainWatcherGapBacklog(ctx, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	rows, err := store.pool.Query(ctx, `SELECT id,from_timestamp,to_timestamp,start_page,next_page
		FROM chain_watcher_gap_tasks WHERE status='pending' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type gapCursor struct {
		id          int64
		from, to    int64
		start, next int
	}
	var gaps []gapCursor
	for rows.Next() {
		var gap gapCursor
		if err := rows.Scan(&gap.id, &gap.from, &gap.to, &gap.start, &gap.next); err != nil {
			t.Fatal(err)
		}
		gaps = append(gaps, gap)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 2 {
		t.Fatalf("pending gaps = %+v, want progressed task plus one successor", gaps)
	}
	if gaps[0].id != originalID || gaps[0].from != base || gaps[0].to != base+10_000 || gaps[0].next != 7 {
		t.Fatalf("progressed gap changed during merge/normalize: %+v", gaps[0])
	}
	if gaps[1].next != gaps[1].start || gaps[1].from != base+1 || gaps[1].to != base+10_100 {
		t.Fatalf("successor gap = %+v", gaps[1])
	}
}

func TestFairGapClaimRotatesByLastHandledTime(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_fair_rotation")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := now.UnixMilli()
	ids := make([]int64, 0, 3)
	for index := 0; index < 3; index++ {
		id, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
			Kind: "window", Source: "watcher", Priority: 2, Reason: fmt.Sprintf("window-%d", index),
			FromTimestamp: base + int64(index*10_000), ToTimestamp: base + int64(index*10_000+5_000),
		}, now.Add(time.Duration(index-30)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	first, ok, err := store.ClaimChainWatcherGap(ctx, "fair-a", "watcher_fair", time.Minute, now)
	if err != nil || !ok || first.ID != ids[0] {
		t.Fatalf("first fair claim = %+v/%v/%v, want %d", first, ok, err, ids[0])
	}
	if yielded, err := store.YieldChainWatcherGap(ctx, first.ID, first.LeaseGeneration, first.LeaseOwner, first.NextPage, "", now); err != nil || !yielded {
		t.Fatalf("yield first = %v/%v", yielded, err)
	}
	second, ok, err := store.ClaimChainWatcherGap(ctx, "fair-b", "watcher_fair", time.Minute, now.Add(time.Second))
	if err != nil || !ok || second.ID != ids[1] {
		t.Fatalf("second fair claim = %+v/%v/%v, want %d instead of immediately reclaiming %d", second, ok, err, ids[1], ids[0])
	}
}

func TestRepeatedDynamicPageFailuresRemainPreciselyDeduplicated(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "gap_timeout_bound")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	base := now.UnixMilli()
	pagesByRound := []int{1, 4, 2}
	for replay := 0; replay < 10; replay++ {
		for round, pages := range pagesByRound {
			cutoff := base + int64(round+1)*1000
			for page := 0; page < pages; page++ {
				if _, err := store.EnqueueChainWatcherGap(ctx, ChainWatcherGapTask{
					Kind: "page", Source: "watcher", Priority: 0, Reason: "deadline",
					FromTimestamp: base - 600_000, ToTimestamp: cutoff,
					StartPage: page, EndPage: page + 1, NextPage: page,
				}, now); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	stats, err := store.ChainWatcherGapStats(ctx, now)
	if err != nil || stats.PendingCount != 7 {
		t.Fatalf("dynamic timeout gap stats = %+v/%v, want 7 pending", stats, err)
	}
}

func TestRealtimeWatermarkRejectsOlderCompletedRound(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	migrationURL, _, _ := postgresTestSchema(t, ctx, dsn, "realtime_watermark_order")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	newer := now.UnixMilli()
	advanced, err := store.AdvanceChainWatcherRealtimeWatermark(ctx, newer, "newer-anchor", now)
	if err != nil || !advanced {
		t.Fatalf("newer watermark = %v/%v", advanced, err)
	}
	advanced, err = store.AdvanceChainWatcherRealtimeWatermark(ctx, newer-1000, "older-anchor", now.Add(time.Second))
	if err != nil || advanced {
		t.Fatalf("older watermark = %v/%v, want rejected", advanced, err)
	}
	watermark, err := store.GetChainWatcherRealtimeWatermark(ctx)
	if err != nil || watermark.Timestamp != newer || watermark.TxHash != "newer-anchor" {
		t.Fatalf("watermark after old round = %+v/%v", watermark, err)
	}
}
