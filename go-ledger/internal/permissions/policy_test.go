package permissions

import "testing"

func TestPolicyPrivilegedUsers(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{
		2002: {},
		0:    {},
	})

	if !p.IsHost(1001) {
		t.Fatal("host should be recognized")
	}
	if !p.IsDefaultOperator(2002) {
		t.Fatal("default operator should be recognized")
	}
	if !p.IsPrivileged(1001) || !p.IsPrivileged(2002) {
		t.Fatal("host and default operator should be privileged")
	}
	if p.IsPrivileged(3003) {
		t.Fatal("ordinary user should not be privileged")
	}
	if p.IsDefaultOperator(0) {
		t.Fatal("zero user id should never be a default operator")
	}
}

func TestPolicyGlobalCapabilitiesUsePrivilegedUsers(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})

	for _, userID := range []int64{1001, 2002} {
		if !p.CanInviteBot(userID, UserCapabilities{}) {
			t.Fatalf("%d should be allowed to invite bot", userID)
		}
		if !p.HasGlobalLedgerAccess(userID) {
			t.Fatalf("%d should have global ledger access", userID)
		}
		if !p.HasGlobalBroadcastAccess(userID) {
			t.Fatalf("%d should have global broadcast access", userID)
		}
		if !p.HasGlobalAddressWatchAccess(userID) {
			t.Fatalf("%d should have global address watch access", userID)
		}
		if !p.CanManageAnyGroup(userID) {
			t.Fatalf("%d should manage any group", userID)
		}
	}

	if p.CanInviteBot(3003, UserCapabilities{}) || p.HasGlobalLedgerAccess(3003) ||
		p.HasGlobalBroadcastAccess(3003) || p.HasGlobalAddressWatchAccess(3003) ||
		p.CanManageAnyGroup(3003) {
		t.Fatal("ordinary user should not receive global capabilities")
	}
}

func TestPolicyCanInviteBotUsesGlobalOperatorCapabilitiesOnly(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})

	if p.CanInviteBot(3003, UserCapabilities{}) {
		t.Fatal("single-group ledger operator should not be allowed to invite bot")
	}
	if !p.CanInviteBot(4004, UserCapabilities{GlobalOperatorLevel: "primary"}) {
		t.Fatal("primary global operator should be allowed to invite bot")
	}
	if !p.CanInviteBot(5005, UserCapabilities{GlobalOperatorLevel: "secondary"}) {
		t.Fatal("secondary global operator should be allowed to invite bot")
	}
	if p.CanInviteBot(6006, UserCapabilities{GlobalOperatorLevel: "disabled"}) {
		t.Fatal("invalid or disabled global operator level should not invite bot")
	}
	if !p.CanInviteBot(1001, UserCapabilities{}) {
		t.Fatal("host should be allowed to invite bot")
	}
	if !p.CanInviteBot(2002, UserCapabilities{}) {
		t.Fatal("default operator should be allowed to invite bot")
	}
	if p.CanInviteBot(7007, UserCapabilities{}) {
		t.Fatal("ordinary user without local capabilities should not invite bot")
	}
}

func TestPolicyPrivateGlobalFeaturesUseGlobalOperatorCapabilities(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})

	if !p.CanUsePrivateGlobalFeatures(1001, UserCapabilities{}) {
		t.Fatal("host should use private global features")
	}
	if !p.CanUsePrivateGlobalFeatures(2002, UserCapabilities{}) {
		t.Fatal("default operator should use private global features")
	}
	if !p.CanUsePrivateGlobalFeatures(3003, UserCapabilities{GlobalOperatorLevel: "secondary"}) {
		t.Fatal("secondary global operator should use private global features")
	}
	if p.CanUsePrivateGlobalFeatures(4004, UserCapabilities{}) {
		t.Fatal("ordinary user should not use private global features")
	}
}

func TestPolicyGlobalOperatorsUseLedgerWithoutElevatingSingleGroupOperators(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})

	for _, caps := range []UserCapabilities{
		{GlobalOperatorLevel: "primary"},
		{GlobalOperatorLevel: "secondary", ParentUserID: 3003},
	} {
		if !p.CanUseLedger(4004, caps) {
			t.Fatalf("global operator should use ledger: %+v", caps)
		}
	}
	if p.CanUseLedger(5005, UserCapabilities{}) {
		t.Fatal("single-group capability must not be inferred as global ledger access")
	}
}

func TestPolicyGlobalOperatorDelegationBoundaries(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})
	primary := UserCapabilities{GlobalOperatorLevel: "primary"}
	secondary := UserCapabilities{GlobalOperatorLevel: "secondary", ParentUserID: 3003}

	if !p.CanCreateGlobalOperator(1001, UserCapabilities{}, "primary") || !p.CanCreateGlobalOperator(1001, UserCapabilities{}, "secondary") {
		t.Fatal("host should create primary operators and secondaries for a selected primary")
	}
	if p.CanCreateGlobalOperator(2002, UserCapabilities{}, "primary") || p.CanCreateGlobalOperator(2002, UserCapabilities{}, "secondary") {
		t.Fatal("default operator should not create database global operators")
	}
	if p.CanCreateGlobalOperator(3003, primary, "primary") || !p.CanCreateGlobalOperator(3003, primary, "secondary") {
		t.Fatal("primary should only create secondary operators")
	}
	if p.CanCreateGlobalOperator(4004, secondary, "secondary") {
		t.Fatal("secondary should not delegate")
	}
	if !p.CanDisableGlobalOperator(3003, primary, "secondary", 3003) {
		t.Fatal("primary should disable its own secondary")
	}
	if p.CanDisableGlobalOperator(3003, primary, "secondary", 9999) || p.CanDisableGlobalOperator(3003, primary, "primary", 0) {
		t.Fatal("primary should not disable unrelated or primary operators")
	}
	if !p.CanDisableGlobalOperator(1001, UserCapabilities{}, "primary", 0) || !p.CanDisableGlobalOperator(1001, UserCapabilities{}, "secondary", 3003) {
		t.Fatal("host should disable primary and secondary operators")
	}
}

func TestPolicyBroadcastPermissionManagers(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})
	if !p.CanManageBroadcastPermissions(1001, UserCapabilities{}) || !p.CanManageAllBroadcastPermissions(1001) {
		t.Fatal("host should manage all broadcast permissions")
	}
	if !p.CanManageBroadcastPermissions(2002, UserCapabilities{}) || !p.CanManageAllBroadcastPermissions(2002) {
		t.Fatal("default operator should manage all broadcast permissions")
	}
	if !p.CanManageBroadcastPermissions(3003, UserCapabilities{GlobalOperatorLevel: "primary"}) || p.CanManageAllBroadcastPermissions(3003) {
		t.Fatal("primary should manage only delegated broadcast permissions")
	}
	if p.CanManageBroadcastPermissions(4004, UserCapabilities{GlobalOperatorLevel: "secondary"}) {
		t.Fatal("secondary should not grant broadcast permissions")
	}
}

func TestPolicyBroadcastGroupOwnershipAndDelegation(t *testing.T) {
	p := NewPolicy(1001, map[int64]struct{}{2002: {}})
	primary := UserCapabilities{GlobalOperatorLevel: "primary"}
	secondary := UserCapabilities{GlobalOperatorLevel: "secondary", ParentUserID: 3003}

	if !p.CanManageBroadcastGroups(1001, UserCapabilities{}) {
		t.Fatal("host should manage all broadcast groups")
	}
	if p.CanManageBroadcastGroups(2002, UserCapabilities{}) {
		t.Fatal("default operator should retain broadcast use/grant access without group ownership management")
	}
	if !p.CanManageBroadcastGroups(3003, primary) || p.CanManageBroadcastGroups(4004, secondary) {
		t.Fatal("only host and primary global operators should manage broadcast groups")
	}
	if !p.CanManageBroadcastGroup(3003, primary, 3003) || p.CanManageBroadcastGroup(3003, primary, 5005) {
		t.Fatal("primary should manage only groups it owns")
	}
	if !p.CanManageBroadcastGroup(1001, UserCapabilities{}, 5005) {
		t.Fatal("host should manage groups regardless of owner")
	}
	if !p.CanTransferBroadcastGroupOwner(1001) {
		t.Fatal("host should transfer broadcast group ownership")
	}
	for _, userID := range []int64{2002, 3003, 4004} {
		if p.CanTransferBroadcastGroupOwner(userID) {
			t.Fatalf("non-host %d should not transfer broadcast group ownership", userID)
		}
	}

	if !p.CanDelegateBroadcastPermission(3003, primary, 4004, "primary", 0) {
		t.Fatal("primary should grant broadcast use to another primary")
	}
	if !p.CanDelegateBroadcastPermission(3003, primary, 5005, "secondary", 3003) {
		t.Fatal("primary should grant broadcast use to its own secondary")
	}
	if p.CanDelegateBroadcastPermission(3003, primary, 5006, "secondary", 4004) {
		t.Fatal("primary should not grant to another primary's secondary")
	}
	if p.CanDelegateBroadcastPermission(5005, secondary, 4004, "primary", 0) {
		t.Fatal("secondary should not delegate broadcast permissions")
	}
}
