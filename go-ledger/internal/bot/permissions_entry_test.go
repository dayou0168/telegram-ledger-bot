package bot

import (
	"context"
	"testing"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func testGlobalOperatorBot(level string, active bool) *Bot {
	return &Bot{
		perms: permissions.NewPolicy(1001, map[int64]struct{}{2002: {}}),
		globalOperatorLookup: func(context.Context, int64) (permissions.UserCapabilities, bool, error) {
			if !active {
				return permissions.UserCapabilities{}, false, nil
			}
			return permissions.UserCapabilities{GlobalOperatorLevel: level}, true, nil
		},
	}
}

func TestActiveGlobalOperatorEntryCapabilities(t *testing.T) {
	ctx := context.Background()
	for _, level := range []string{"primary", "secondary"} {
		t.Run(level, func(t *testing.T) {
			b := testGlobalOperatorBot(level, true)
			if ok, err := b.canInvite(ctx, 3003); err != nil || !ok {
				t.Fatalf("canInvite = %v, %v", ok, err)
			}
			if !b.canUseBroadcast(ctx, 3003) {
				t.Fatal("active global operator should enter broadcast")
			}
			if !b.hasUnlimitedAddressWatch(ctx, 3003) {
				t.Fatal("active global operator should have unlimited address watch")
			}
			if role, ok, err := b.adminRoleForUser(ctx, 3003); err != nil || !ok || role != adminauth.RoleOperator {
				t.Fatalf("admin role = %q, %v, %v", role, ok, err)
			}
			if ok, err := b.canUseLedgerWithGroup(ctx, storage.Group{ChatID: -1001}, 3003); err != nil || !ok {
				t.Fatalf("ledger capability = %v, %v", ok, err)
			}
			if ok, err := b.canManageGroup(ctx, -1001, 3003); err != nil || !ok {
				t.Fatalf("group management capability = %v, %v", ok, err)
			}
		})
	}
}

func TestDisabledGlobalOperatorLosesEveryGlobalEntry(t *testing.T) {
	ctx := context.Background()
	b := testGlobalOperatorBot("primary", false)
	if ok, err := b.canInvite(ctx, 3003); err != nil || ok {
		t.Fatalf("disabled canInvite = %v, %v", ok, err)
	}
	if b.canUseBroadcast(ctx, 3003) || b.hasUnlimitedAddressWatch(ctx, 3003) {
		t.Fatal("disabled global operator retained broadcast or address privilege")
	}
	if _, ok, err := b.adminRoleForUser(ctx, 3003); err != nil || ok {
		t.Fatalf("disabled admin entry = %v, %v", ok, err)
	}
}

func TestSingleGroupOperatorStaysLocalAndUndoUsesCurrentPermission(t *testing.T) {
	ctx := context.Background()
	active := true
	b := testGlobalOperatorBot("", false)
	b.groupOperatorLookup = func(context.Context, int64, int64) (bool, error) {
		return active, nil
	}
	group := storage.Group{ChatID: -1001}
	if ok, err := b.canUseLedgerForUndo(ctx, group, 3003); err != nil || !ok {
		t.Fatalf("active group operator undo permission = %v, %v", ok, err)
	}
	if ok, err := b.canInvite(ctx, 3003); err != nil || ok {
		t.Fatalf("single-group operator invite = %v, %v", ok, err)
	}
	if b.canUseBroadcast(ctx, 3003) || b.hasUnlimitedAddressWatch(ctx, 3003) {
		t.Fatal("single-group operator gained a private global capability")
	}
	if _, ok, err := b.adminRoleForUser(ctx, 3003); err != nil || ok {
		t.Fatalf("single-group operator admin entry = %v, %v", ok, err)
	}
	active = false
	if ok, err := b.canUseLedgerForUndo(ctx, group, 3003); err != nil || ok {
		t.Fatalf("removed group operator undo permission = %v, %v", ok, err)
	}
}

func TestConfiguredPrivilegedUsersDoNotNeedDatabaseRows(t *testing.T) {
	ctx := context.Background()
	b := testGlobalOperatorBot("", false)
	for _, userID := range []int64{1001, 2002} {
		if ok, err := b.canInvite(ctx, userID); err != nil || !ok {
			t.Fatalf("configured privileged invite %d = %v, %v", userID, ok, err)
		}
		if !b.canUseBroadcast(ctx, userID) || !b.hasUnlimitedAddressWatch(ctx, userID) {
			t.Fatalf("configured privileged user %d lost global capability", userID)
		}
	}
}
