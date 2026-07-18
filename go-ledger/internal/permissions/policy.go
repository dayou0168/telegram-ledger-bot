package permissions

type Policy struct {
	hostUserID         int64
	defaultOperatorIDs map[int64]struct{}
}

type UserCapabilities struct {
	GlobalOperatorLevel string
	ParentUserID        int64
}

func NewPolicy(hostUserID int64, defaultOperatorIDs map[int64]struct{}) Policy {
	ids := make(map[int64]struct{}, len(defaultOperatorIDs))
	for id := range defaultOperatorIDs {
		if id != 0 {
			ids[id] = struct{}{}
		}
	}
	return Policy{
		hostUserID:         hostUserID,
		defaultOperatorIDs: ids,
	}
}

func (p Policy) HostUserID() int64 {
	return p.hostUserID
}

func (p Policy) IsHost(userID int64) bool {
	return p.hostUserID != 0 && userID == p.hostUserID
}

func (p Policy) IsDefaultOperator(userID int64) bool {
	if userID == 0 {
		return false
	}
	_, ok := p.defaultOperatorIDs[userID]
	return ok
}

func (p Policy) IsPrivileged(userID int64) bool {
	return p.IsHost(userID) || p.IsDefaultOperator(userID)
}

func (p Policy) PrivilegedUserIDs() []int64 {
	ids := make([]int64, 0, len(p.defaultOperatorIDs)+1)
	if p.hostUserID != 0 {
		ids = append(ids, p.hostUserID)
	}
	for id := range p.defaultOperatorIDs {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p Policy) CanInviteBot(userID int64, caps UserCapabilities) bool {
	return p.IsPrivileged(userID) || caps.IsGlobalOperator()
}

func (p Policy) CanUsePrivateGlobalFeatures(userID int64, caps UserCapabilities) bool {
	return p.IsPrivileged(userID) || caps.IsGlobalOperator()
}

func (p Policy) CanUseLedger(userID int64, caps UserCapabilities) bool {
	return p.IsPrivileged(userID) || caps.IsGlobalOperator()
}

func (p Policy) CanCreateGlobalOperator(userID int64, caps UserCapabilities, targetLevel string) bool {
	if p.IsHost(userID) {
		return targetLevel == "primary" || targetLevel == "secondary"
	}
	return caps.GlobalOperatorLevel == "primary" && targetLevel == "secondary"
}

func (p Policy) CanDisableGlobalOperator(userID int64, caps UserCapabilities, targetLevel string, targetParentUserID int64) bool {
	if p.IsHost(userID) {
		return targetLevel == "primary" || targetLevel == "secondary"
	}
	return caps.GlobalOperatorLevel == "primary" && targetLevel == "secondary" && targetParentUserID == userID
}

func (p Policy) CanManageBroadcastPermissions(userID int64, caps UserCapabilities) bool {
	return p.IsPrivileged(userID) || caps.GlobalOperatorLevel == "primary"
}

func (p Policy) CanManageBroadcastGroups(userID int64, caps UserCapabilities) bool {
	return p.IsHost(userID) || caps.GlobalOperatorLevel == "primary"
}

func (p Policy) CanManageBroadcastGroup(userID int64, caps UserCapabilities, ownerUserID int64) bool {
	return p.IsHost(userID) || (caps.GlobalOperatorLevel == "primary" && ownerUserID == userID)
}

func (p Policy) CanTransferBroadcastGroupOwner(userID int64) bool {
	return p.IsHost(userID)
}

func (p Policy) CanDelegateBroadcastPermission(userID int64, caps UserCapabilities, subjectUserID int64, subjectLevel string, subjectParentUserID int64) bool {
	if p.IsPrivileged(userID) {
		return subjectLevel == "primary" || subjectLevel == "secondary"
	}
	if caps.GlobalOperatorLevel != "primary" {
		return false
	}
	return (subjectLevel == "primary" && subjectUserID != userID) ||
		(subjectLevel == "secondary" && subjectParentUserID == userID)
}

func (p Policy) CanManageAllBroadcastPermissions(userID int64) bool {
	return p.IsPrivileged(userID)
}

func (p Policy) CanManageGlobalAdmin(userID int64) bool {
	return p.IsHost(userID)
}

func (p Policy) CanManageMessageObservers(userID int64) bool {
	return p.IsHost(userID)
}

func (caps UserCapabilities) IsGlobalOperator() bool {
	return caps.GlobalOperatorLevel == "primary" || caps.GlobalOperatorLevel == "secondary"
}

func (p Policy) HasGlobalLedgerAccess(userID int64) bool {
	return p.IsPrivileged(userID)
}

func (p Policy) HasGlobalBroadcastAccess(userID int64) bool {
	return p.IsPrivileged(userID)
}

func (p Policy) HasGlobalAddressWatchAccess(userID int64) bool {
	return p.IsPrivileged(userID)
}

func (p Policy) CanManageAnyGroup(userID int64) bool {
	return p.IsPrivileged(userID)
}
