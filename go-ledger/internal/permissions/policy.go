package permissions

type Policy struct {
	HostUserID         int64
	DefaultOperatorIDs map[int64]struct{}
}

func NewPolicy(hostUserID int64, defaultOperatorIDs map[int64]struct{}) Policy {
	ids := make(map[int64]struct{}, len(defaultOperatorIDs))
	for id := range defaultOperatorIDs {
		if id != 0 {
			ids[id] = struct{}{}
		}
	}
	return Policy{
		HostUserID:         hostUserID,
		DefaultOperatorIDs: ids,
	}
}

func (p Policy) IsHost(userID int64) bool {
	return p.HostUserID != 0 && userID == p.HostUserID
}

func (p Policy) IsDefaultOperator(userID int64) bool {
	if userID == 0 {
		return false
	}
	_, ok := p.DefaultOperatorIDs[userID]
	return ok
}

func (p Policy) IsPrivileged(userID int64) bool {
	return p.IsHost(userID) || p.IsDefaultOperator(userID)
}

func (p Policy) PrivilegedUserIDs() []int64 {
	ids := make([]int64, 0, len(p.DefaultOperatorIDs)+1)
	if p.HostUserID != 0 {
		ids = append(ids, p.HostUserID)
	}
	for id := range p.DefaultOperatorIDs {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func (p Policy) CanInviteBot(userID int64) bool {
	return p.IsPrivileged(userID)
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
