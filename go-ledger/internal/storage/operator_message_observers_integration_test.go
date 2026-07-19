package storage

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func TestPostgresOperatorMessageObserversMigrationAndRecipientContract(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	migrationURL, admin, quotedSchema := postgresTestSchema(t, ctx, dsn, "operator_message_observers")
	store, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	var markerCount, grantTableCount, auditTableCount int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM "+quotedSchema+".schema_migrations WHERE version=$1",
		operatorMessageObserversMigrationVersion).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_tables
		WHERE schemaname=$1 AND tablename='operator_message_observer_grants'`,
		quotedSchema[1:len(quotedSchema)-1]).Scan(&grantTableCount); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_tables
		WHERE schemaname=$1 AND tablename='operator_message_observer_audit_events'`,
		quotedSchema[1:len(quotedSchema)-1]).Scan(&auditTableCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 || grantTableCount != 1 || auditTableCount != 1 {
		t.Fatalf("migration marker=%d grants=%d audit=%d", markerCount, grantTableCount, auditTableCount)
	}
	store.Close()
	for _, statement := range []string{
		"DROP TRIGGER IF EXISTS trg_revoke_operator_message_observers ON " + quotedSchema + ".global_operators",
		"DROP TABLE " + quotedSchema + ".operator_message_observer_audit_events",
		"DROP TABLE " + quotedSchema + ".operator_message_observer_grants",
		"DELETE FROM " + quotedSchema + ".schema_migrations WHERE version IN ('" + operatorMessageObserversMigrationVersion + "','" + latestSchemaMigrationVersion + "')",
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("prepare prior-schema fixture: %v", err)
		}
	}
	store, err = Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("upgrade prior schema: %v", err)
	}
	defer store.Close()
	reopened, err := Open(ctx, migrationURL)
	if err != nil {
		t.Fatalf("idempotent open: %v", err)
	}
	reopened.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	hostID := int64(700000420001)
	primaryAID := int64(700000420002)
	primaryBID := int64(700000420003)
	primaryCID := int64(700000420004)
	secondaryID := int64(700000420005)
	for _, op := range []struct {
		userID   int64
		level    string
		parentID int64
	}{
		{primaryAID, "primary", 0},
		{primaryBID, "primary", 0},
		{primaryCID, "primary", 0},
		{secondaryID, "secondary", primaryAID},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, hostID, "observer fixture", now); err != nil {
			t.Fatalf("upsert global operator %d: %v", op.userID, err)
		}
	}

	if changed, err := store.UpsertOperatorMessageObserverGrant(
		ctx, secondaryID, primaryBID, true, false, hostID, now.Add(time.Second),
	); err != nil || !changed {
		t.Fatalf("grant broadcast observer changed=%v err=%v", changed, err)
	}
	if changed, err := store.UpsertOperatorMessageObserverGrant(
		ctx, secondaryID, primaryCID, false, true, hostID, now.Add(2*time.Second),
	); err != nil || !changed {
		t.Fatalf("grant reply observer changed=%v err=%v", changed, err)
	}
	if changed, err := store.UpsertOperatorMessageObserverGrant(
		ctx, secondaryID, primaryBID, true, false, hostID, now.Add(3*time.Second),
	); err != nil || changed {
		t.Fatalf("idempotent grant changed=%v err=%v", changed, err)
	}
	if _, err := store.UpsertOperatorMessageObserverGrant(ctx, secondaryID, primaryAID, true, true, hostID, now); !errors.Is(err, ErrMessageObserverInvalidScope) {
		t.Fatalf("direct parent grant error=%v", err)
	}
	if _, err := store.UpsertOperatorMessageObserverGrant(ctx, primaryAID, primaryBID, true, true, hostID, now); !errors.Is(err, ErrMessageObserverInvalidScope) {
		t.Fatalf("primary source grant error=%v", err)
	}
	if _, err := store.UpsertOperatorMessageObserverGrant(ctx, secondaryID, primaryBID, false, false, hostID, now); !errors.Is(err, ErrMessageObserverNoChannels) {
		t.Fatalf("empty channel grant error=%v", err)
	}

	recipients, err := store.ResolveOperatorMessageRecipients(ctx, secondaryID, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{hostID, primaryAID, primaryBID}; !reflect.DeepEqual(recipients.Broadcast, want) {
		t.Fatalf("broadcast recipients=%v want=%v", recipients.Broadcast, want)
	}
	if want := []int64{hostID, primaryAID, primaryCID}; !reflect.DeepEqual(recipients.Reply, want) {
		t.Fatalf("reply recipients=%v want=%v", recipients.Reply, want)
	}
	assertSourceScope := func(observerID int64, sourceID int64, wantBroadcast, wantReply bool) {
		t.Helper()
		scopes, scopeErr := store.ListOperatorMessageSourcesForObserver(ctx, observerID, hostID)
		if scopeErr != nil {
			t.Fatal(scopeErr)
		}
		for _, scope := range scopes {
			if scope.SourceUserID == sourceID {
				if scope.AllowBroadcast != wantBroadcast || scope.AllowReply != wantReply {
					t.Fatalf("observer=%d scope=%+v want broadcast=%t reply=%t", observerID, scope, wantBroadcast, wantReply)
				}
				return
			}
		}
		t.Fatalf("observer=%d missing source=%d scopes=%+v", observerID, sourceID, scopes)
	}
	assertSourceScope(hostID, secondaryID, true, true)
	assertSourceScope(primaryAID, secondaryID, true, true)
	assertSourceScope(primaryBID, secondaryID, true, false)
	assertSourceScope(primaryCID, secondaryID, false, true)

	if changed, err := store.RevokeOperatorMessageObserverGrant(ctx, secondaryID, primaryCID, hostID, now.Add(4*time.Second)); err != nil || !changed {
		t.Fatalf("revoke changed=%v err=%v", changed, err)
	}
	if changed, err := store.RevokeOperatorMessageObserverGrant(ctx, secondaryID, primaryCID, hostID, now.Add(5*time.Second)); err != nil || changed {
		t.Fatalf("idempotent revoke changed=%v err=%v", changed, err)
	}
	if disabled, err := store.DisableGlobalOperator(ctx, primaryBID, hostID, now.Add(6*time.Second)); err != nil || !disabled {
		t.Fatalf("disable observer=%v err=%v", disabled, err)
	}
	recipients, err = store.ResolveOperatorMessageRecipients(ctx, secondaryID, hostID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{hostID, primaryAID}; !reflect.DeepEqual(recipients.Broadcast, want) || !reflect.DeepEqual(recipients.Reply, want) {
		t.Fatalf("recipients after revoke/disable=%+v want=%v", recipients, want)
	}
	if scopes, err := store.ListOperatorMessageSourcesForObserver(ctx, primaryBID, hostID); err != nil || len(scopes) != 0 {
		t.Fatalf("disabled observer scopes=%+v err=%v", scopes, err)
	}

	grants, err := store.ListOperatorMessageObserverGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 2 || grants[0].Active || grants[1].Active {
		t.Fatalf("inactive grants=%+v", grants)
	}
	events, err := store.ListOperatorMessageObserverAuditEvents(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]int{}
	for _, event := range events {
		actions[event.Action]++
		if event.ActorUserID != hostID {
			t.Fatalf("audit actor=%d want host=%d", event.ActorUserID, hostID)
		}
	}
	if actions["granted"] != 2 || actions["revoked"] != 1 || actions["identity_disabled"] != 1 {
		t.Fatalf("audit actions=%v events=%+v", actions, events)
	}
	if _, err := admin.Exec(ctx, "UPDATE "+quotedSchema+".operator_message_observer_audit_events SET action='updated'"); err == nil {
		t.Fatal("observer audit must be immutable")
	}
}
