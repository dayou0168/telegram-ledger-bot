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
