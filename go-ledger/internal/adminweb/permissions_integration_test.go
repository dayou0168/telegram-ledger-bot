package adminweb

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestAdminGlobalOperatorDisableAndDelegatedBroadcastScope(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := int64(880000000000 + now.UnixNano()%1000000)
	if base <= 1<<31-1 {
		t.Fatalf("integration user id %d must exceed PostgreSQL int4", base)
	}
	primaryID := base
	secondaryID := base + 1
	otherPrimaryID := base + 2
	otherSecondaryID := base + 3
	chatID := -base
	if err := store.UpsertGlobalOperator(ctx, primaryID, "primary", 0, 1001, "primary", now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertGlobalOperator(ctx, secondaryID, "secondary", primaryID, primaryID, "secondary", now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertGlobalOperator(ctx, otherPrimaryID, "primary", 0, 1001, "other primary", now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertGlobalOperator(ctx, otherSecondaryID, "secondary", otherPrimaryID, otherPrimaryID, "other secondary", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatID, "delegated chat", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryID, "chat", chatID, "", 1001, now); err != nil {
		t.Fatal(err)
	}

	s := New(config.Config{
		HostUserID:         1001,
		DefaultOperatorIDs: map[int64]struct{}{2002: {}},
	}, store)
	primarySession := adminauth.Session{UserID: primaryID, Role: adminauth.RoleOperator}
	if allowed, err := s.adminSessionAllowed(ctx, primarySession); err != nil || !allowed {
		t.Fatalf("primary admin session = %v, %v", allowed, err)
	}
	if allowed, err := s.canMutateBroadcastPermission(ctx, primarySession, secondaryID, "chat", chatID, "", true); err != nil || !allowed {
		t.Fatalf("delegated grant = %v, %v", allowed, err)
	}
	if allowed, err := s.canMutateBroadcastPermission(ctx, primarySession, secondaryID, "chat", chatID-1, "", true); err != nil || allowed {
		t.Fatalf("out-of-scope grant = %v, %v", allowed, err)
	}
	if allowed, err := s.canMutateBroadcastPermission(ctx, primarySession, otherSecondaryID, "chat", chatID, "", true); err != nil || allowed {
		t.Fatalf("unrelated secondary grant = %v, %v", allowed, err)
	}
	secondarySession := adminauth.Session{UserID: secondaryID, Role: adminauth.RoleOperator}
	if allowed, err := s.canMutateBroadcastPermission(ctx, secondarySession, secondaryID, "chat", chatID, "", true); err != nil || allowed {
		t.Fatalf("secondary delegation = %v, %v", allowed, err)
	}
	defaultSession := adminauth.Session{UserID: 2002, Role: adminauth.RoleDefaultOperator}
	if allowed, err := s.canMutateBroadcastPermission(ctx, defaultSession, otherSecondaryID, "chat", chatID-1, "", true); err != nil || !allowed {
		t.Fatalf("default unrestricted grant = %v, %v", allowed, err)
	}

	if _, err := store.DisableGlobalOperator(ctx, secondaryID, primaryID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if allowed, err := s.adminSessionAllowed(ctx, secondarySession); err != nil || allowed {
		t.Fatalf("disabled secondary admin session = %v, %v", allowed, err)
	}
}

func TestAdminGlobalOperatorHierarchyHandlersAndSelectors(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := int64(930000000000 + now.UnixNano()%1000000)
	hostID := base
	defaultID := base + 1
	primaryID := base + 2
	otherPrimaryID := base + 3
	secondaryID := base + 4
	otherSecondaryID := base + 5
	disabledSecondaryID := base + 6
	chatID := -base
	groupName := fmt.Sprintf("scope-%d", base)
	for _, op := range []struct {
		userID, parentID, createdBy int64
		level, remark               string
	}{
		{hostID, 0, 0, "primary", "environment host shadow"},
		{defaultID, 0, 0, "primary", "environment default shadow"},
		{primaryID, 0, hostID, "primary", "primary A"},
		{otherPrimaryID, 0, hostID, "primary", "primary B"},
		{secondaryID, primaryID, primaryID, "secondary", "secondary A"},
		{otherSecondaryID, otherPrimaryID, otherPrimaryID, "secondary", "secondary B"},
		{disabledSecondaryID, primaryID, primaryID, "secondary", "disabled secondary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, op.createdBy, op.remark, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DisableGlobalOperator(ctx, disabledSecondaryID, primaryID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatID, "permission target", now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBroadcastGroup(ctx, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddChatsToBroadcastGroup(ctx, groupName, []int64{chatID}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryID, "group", 0, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryID, "chat", chatID, "", hostID, now); err != nil {
		t.Fatal(err)
	}

	s := New(config.Config{
		HostUserID:         hostID,
		DefaultOperatorIDs: map[int64]struct{}{defaultID: {}},
	}, store)
	hostSession := adminauth.Session{UserID: hostID, Role: adminauth.RoleHost}
	defaultSession := adminauth.Session{UserID: defaultID, Role: adminauth.RoleDefaultOperator}
	primarySession := adminauth.Session{UserID: primaryID, Role: adminauth.RoleOperator}
	otherPrimarySession := adminauth.Session{UserID: otherPrimaryID, Role: adminauth.RoleOperator}
	secondarySession := adminauth.Session{UserID: secondaryID, Role: adminauth.RoleOperator}

	for _, tc := range []struct {
		name      string
		session   adminauth.Session
		subjectID int64
		target    string
		chatID    int64
		groupName string
		want      bool
	}{
		{"host chat", hostSession, secondaryID, "chat", chatID, "", true},
		{"default group", defaultSession, otherSecondaryID, "group", 0, groupName, true},
		{"primary own chat", primarySession, secondaryID, "chat", chatID, "", true},
		{"primary own group", primarySession, secondaryID, "group", 0, groupName, true},
		{"primary cross parent", primarySession, otherSecondaryID, "chat", chatID, "", false},
		{"secondary cannot grant", secondarySession, secondaryID, "chat", chatID, "", false},
		{"host shadow cannot receive", hostSession, hostID, "chat", chatID, "", false},
		{"default shadow cannot receive", hostSession, defaultID, "group", 0, groupName, false},
		{"disabled cannot receive", hostSession, disabledSecondaryID, "chat", chatID, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.canMutateBroadcastPermission(ctx, tc.session, tc.subjectID, tc.target, tc.chatID, tc.groupName, true)
			if err != nil || got != tc.want {
				t.Fatalf("allowed=%v err=%v, want %v", got, err, tc.want)
			}
		})
	}

	operatorIDs := func(operators []storage.GlobalOperator) map[int64]bool {
		ids := make(map[int64]bool, len(operators))
		for _, op := range operators {
			ids[op.UserID] = true
		}
		return ids
	}
	assertSelector := func(name string, session adminauth.Session, want, blocked []int64) pageData {
		t.Helper()
		data, err := s.loadPageData(ctx, "", session)
		if err != nil {
			t.Fatalf("%s load page: %v", name, err)
		}
		ids := operatorIDs(data.PermissionOperators)
		for _, userID := range want {
			if !ids[userID] {
				t.Errorf("%s selector missing %d: %v", name, userID, ids)
			}
		}
		for _, userID := range blocked {
			if ids[userID] {
				t.Errorf("%s selector exposed %d: %v", name, userID, ids)
			}
		}
		return data
	}
	assertSelector("host", hostSession,
		[]int64{primaryID, otherPrimaryID, secondaryID, otherSecondaryID},
		[]int64{hostID, defaultID, disabledSecondaryID})
	assertSelector("default", defaultSession,
		[]int64{primaryID, otherPrimaryID, secondaryID, otherSecondaryID},
		[]int64{hostID, defaultID, disabledSecondaryID})
	primaryPage := assertSelector("primary", primarySession,
		[]int64{secondaryID, otherPrimaryID},
		[]int64{hostID, defaultID, primaryID, otherSecondaryID, disabledSecondaryID})
	if ids := operatorIDs(primaryPage.BOperators); !ids[secondaryID] || !ids[disabledSecondaryID] || ids[otherSecondaryID] {
		t.Fatalf("primary managed operator list = %v", ids)
	}

	post := func(session adminauth.Session, path, form string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(req.Context(), adminContextKey{}, session))
		handler(rec, req)
		return rec
	}
	newPrimaryID := base + 10
	if rec := post(hostSession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary", newPrimaryID), s.saveOperator); rec.Code != http.StatusSeeOther {
		t.Fatalf("host secondary without parent validation: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, exists, err := store.GetGlobalOperator(ctx, newPrimaryID); err != nil || exists {
		t.Fatalf("secondary without selected primary was persisted: exists=%v err=%v", exists, err)
	}
	if rec := post(hostSession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary&parent_user_id=%d", newPrimaryID, secondaryID), s.saveOperator); rec.Code != http.StatusForbidden {
		t.Fatalf("host selected non-primary parent: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(hostSession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=primary&remark=new", newPrimaryID), s.saveOperator); rec.Code != http.StatusSeeOther {
		t.Fatalf("host create primary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	hostCreatedSecondaryID := base + 13
	if rec := post(hostSession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary&parent_user_id=%d&remark=delegated", hostCreatedSecondaryID, primaryID), s.saveOperator); rec.Code != http.StatusSeeOther {
		t.Fatalf("host create secondary for primary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if op, ok, err := store.GetGlobalOperator(ctx, hostCreatedSecondaryID); err != nil || !ok || op.Level != "secondary" || op.ParentUserID != primaryID {
		t.Fatalf("host-created secondary = %+v ok=%v err=%v", op, ok, err)
	}
	newSecondaryID := base + 11
	if rec := post(primarySession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary&remark=new", newSecondaryID), s.saveOperator); rec.Code != http.StatusSeeOther {
		t.Fatalf("primary create secondary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(secondarySession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary", base+12), s.saveOperator); rec.Code != http.StatusForbidden {
		t.Fatalf("secondary delegated: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(otherPrimarySession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary", secondaryID), s.saveOperator); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-parent update: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primarySession, "/admin/permission/grant", fmt.Sprintf("user_id=%d&target=group&group_name=%s", secondaryID, groupName), s.grantPermission); rec.Code != http.StatusSeeOther {
		t.Fatalf("primary group grant: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primarySession, "/admin/permission/grant", fmt.Sprintf("user_id=%d&target=chat&chat_id=%d", secondaryID, chatID), s.grantPermission); rec.Code != http.StatusSeeOther {
		t.Fatalf("primary chat grant: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primarySession, "/admin/permission/grant", fmt.Sprintf("user_id=%d&target=chat&chat_id=%d", otherPrimaryID, chatID), s.grantPermission); rec.Code != http.StatusSeeOther {
		t.Fatalf("primary to peer-primary chat grant: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primarySession, "/admin/operator/disable", fmt.Sprintf("user_id=%d", secondaryID), s.disableOperator); rec.Code != http.StatusSeeOther {
		t.Fatalf("disable own secondary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertSelector("disabled secondary", primarySession, nil, []int64{secondaryID})
	if rec := post(primarySession, "/admin/operator/save", fmt.Sprintf("user_id=%d&level=secondary&remark=reenabled", secondaryID), s.saveOperator); rec.Code != http.StatusSeeOther {
		t.Fatalf("reenable own secondary: status=%d body=%s", rec.Code, rec.Body.String())
	}
	assertSelector("reenabled secondary", primarySession, []int64{secondaryID}, nil)
	if allowed, err := store.HasBroadcastPermissionScope(ctx, secondaryID, "chat", chatID, ""); err != nil || !allowed {
		t.Fatalf("reenable did not restore chat permission: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, secondaryID, "group", 0, groupName); err != nil || !allowed {
		t.Fatalf("reenable did not restore group permission: allowed=%v err=%v", allowed, err)
	}
}

func TestAdminPrimaryBroadcastGroupOwnershipAndForgedPOSTBoundaries(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	base := int64(960000000000 + now.UnixNano()%1000000)
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
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, op.createdBy, "handler fixture", now); err != nil {
			t.Fatal(err)
		}
	}
	for _, chatID := range []int64{chatAID, chatBID} {
		if err := store.EnsureGroup(ctx, chatID, fmt.Sprintf("handler %d", chatID), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.AddBroadcastPermission(ctx, primaryAID, "chat", chatAID, "", hostID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryBID, "chat", chatBID, "", hostID, now); err != nil {
		t.Fatal(err)
	}

	s := New(config.Config{HostUserID: hostID}, store)
	primaryASession := adminauth.Session{UserID: primaryAID, Role: adminauth.RoleOperator}
	primaryBSession := adminauth.Session{UserID: primaryBID, Role: adminauth.RoleOperator}
	post := func(session adminauth.Session, path, form string, handler http.HandlerFunc) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(req.Context(), adminContextKey{}, session))
		handler(rec, req)
		return rec
	}

	groupName := fmt.Sprintf("handler-owned-%d", base)
	if rec := post(primaryASession, "/admin/group/save", "name="+groupName, s.saveGroup); rec.Code != http.StatusSeeOther {
		t.Fatalf("primary create group: status=%d body=%s", rec.Code, rec.Body.String())
	}
	group, ok, err := store.GetBroadcastGroup(ctx, groupName)
	if err != nil || !ok || group.OwnerUserID != primaryAID {
		t.Fatalf("created group=%+v ok=%v err=%v", group, ok, err)
	}
	if rec := post(primaryASession, "/admin/group/add", fmt.Sprintf("name=%s&chat_id=%d", groupName, chatAID), s.addGroupChats); rec.Code != http.StatusSeeOther {
		t.Fatalf("owner add chat: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primaryASession, "/admin/group/add", fmt.Sprintf("name=%s&chat_id=%d", groupName, chatBID), s.addGroupChats); rec.Code != http.StatusForbidden {
		t.Fatalf("owner forged out-of-scope add: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primaryBSession, "/admin/group/rename", fmt.Sprintf("old_name=%s&new_name=forged", groupName), s.renameGroup); rec.Code != http.StatusForbidden {
		t.Fatalf("peer forged rename: status=%d body=%s", rec.Code, rec.Body.String())
	}

	for _, tc := range []struct {
		subjectID int64
		target    string
		value     string
	}{
		{secondaryAID, "group", groupName},
		{primaryBID, "group", groupName},
		{secondaryAID, "chat", fmt.Sprint(chatAID)},
		{primaryBID, "chat", fmt.Sprint(chatAID)},
	} {
		form := fmt.Sprintf("user_id=%d&target=%s", tc.subjectID, tc.target)
		if tc.target == "group" {
			form += "&group_name=" + tc.value
		} else {
			form += "&chat_id=" + tc.value
		}
		if rec := post(primaryASession, "/admin/permission/grant", form, s.grantPermission); rec.Code != http.StatusSeeOther {
			t.Fatalf("grant %+v: status=%d body=%s", tc, rec.Code, rec.Body.String())
		}
	}
	if rec := post(primaryASession, "/admin/permission/grant", fmt.Sprintf("user_id=%d&target=chat&chat_id=%d", secondaryBID, chatAID), s.grantPermission); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-parent forged grant: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post(primaryBSession, "/admin/group/delete", "name="+groupName, s.deleteGroup); rec.Code != http.StatusForbidden {
		t.Fatalf("use-only peer deleted group: status=%d body=%s", rec.Code, rec.Body.String())
	}

	pageA, err := s.loadPageData(ctx, "", primaryASession)
	if err != nil {
		t.Fatal(err)
	}
	if len(pageA.Groups) != 1 || pageA.Groups[0].ChatID != chatAID {
		t.Fatalf("primary A direct chats=%+v", pageA.Groups)
	}
	if len(pageA.BOperators) != 1 || pageA.BOperators[0].UserID != secondaryAID {
		t.Fatalf("primary A managed operators=%+v", pageA.BOperators)
	}
	for _, permission := range pageA.Permissions {
		if permission.GrantedBy != primaryAID && permission.UserID != primaryAID {
			t.Fatalf("primary page exposed unrelated grant: %+v", permission)
		}
	}
	pageB, err := s.loadPageData(ctx, "", primaryBSession)
	if err != nil {
		t.Fatal(err)
	}
	foundUseOnly := false
	for _, candidate := range pageB.BGroups {
		if candidate.Name == groupName {
			foundUseOnly = candidate.OwnerUserID == primaryAID
		}
	}
	if !foundUseOnly {
		t.Fatalf("peer page missing authorized use-only group: %+v", pageB.BGroups)
	}
}

func TestAdminHostBroadcastGroupOwnerTransferBoundariesAndTemplateHooks(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC().Truncate(time.Microsecond)
	base := int64(980000000000 + now.UnixNano()%1000000)
	hostID := base
	primaryAID := base + 1
	primaryBID := base + 2
	secondaryID := base + 3
	disabledPrimaryID := base + 4
	chatID := -base
	for _, op := range []struct {
		userID, parentID, createdBy int64
		level, remark               string
	}{
		{primaryAID, 0, hostID, "primary", "新一"},
		{primaryBID, 0, hostID, "primary", "河马"},
		{secondaryID, primaryAID, primaryAID, "secondary", "下级"},
		{disabledPrimaryID, 0, hostID, "primary", "已禁用一级"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.userID, op.level, op.parentID, op.createdBy, op.remark, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DisableGlobalOperator(ctx, disabledPrimaryID, hostID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatID, "transfer handler chat", now); err != nil {
		t.Fatal(err)
	}
	groupName := fmt.Sprintf("handler-transfer-%d", base)
	if err := store.UpsertBroadcastGroup(ctx, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}
	if added, err := store.AddChatsToBroadcastGroupManaged(ctx, groupName, []int64{chatID}, hostID, true, now); err != nil || added != 1 {
		t.Fatalf("add chat=%d err=%v", added, err)
	}
	if err := store.AddBroadcastPermission(ctx, primaryAID, "group", 0, groupName, hostID, now); err != nil {
		t.Fatal(err)
	}
	hostOwnedName := groupName + "-host"
	if err := store.UpsertBroadcastGroup(ctx, hostOwnedName, hostID, now); err != nil {
		t.Fatal(err)
	}

	invalidator := &testPermissionInvalidator{}
	s := New(config.Config{HostUserID: hostID}, store, invalidator)
	hostSession := adminauth.Session{UserID: hostID, Role: adminauth.RoleHost}
	primarySession := adminauth.Session{UserID: primaryAID, Role: adminauth.RoleOperator}
	post := func(session adminauth.Session, form string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/group/transfer-owner", strings.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(req.Context(), adminContextKey{}, session))
		s.transferGroupOwner(rec, req)
		return rec
	}
	form := func(targetID int64, sync bool) string {
		value := fmt.Sprintf("name=%s&expected_owner_user_id=0&new_owner_user_id=%d", groupName, targetID)
		if sync {
			value += "&sync_missing_permissions=1"
		}
		return value
	}
	if rec := post(primarySession, form(primaryBID, true)); rec.Code != http.StatusForbidden {
		t.Fatalf("forged non-host transfer status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, targetID := range []int64{secondaryID, disabledPrimaryID} {
		if rec := post(hostSession, form(targetID, true)); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid target %d status=%d body=%s", targetID, rec.Code, rec.Body.String())
		}
	}
	if rec := post(hostSession, form(primaryBID, false)); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "1 个") {
		t.Fatalf("missing permission status=%d body=%s", rec.Code, rec.Body.String())
	} else {
		body := rec.Body.String()
		for _, want := range []string{
			`data-initial-tab="broadcast"`,
			`class="msg error"`,
			fmt.Sprintf(`<option value="%s" data-owner-user-id="0" selected>`, groupName),
			fmt.Sprintf(`<option value="%d" selected>河马</option>`, primaryBID),
			`name="expected_owner_user_id" value="0"`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("conflict response missing %q", want)
			}
		}
		transferStart := strings.Index(body, `data-owner-transfer-form`)
		if transferStart < 0 {
			t.Fatal("conflict response missing transfer form")
		}
		transferEnd := strings.Index(body[transferStart:], `</form>`)
		if transferEnd < 0 {
			t.Fatal("conflict response transfer form is incomplete")
		}
		transferHTML := body[transferStart : transferStart+transferEnd]
		if strings.Contains(transferHTML, `name="sync_missing_permissions" value="1" checked`) {
			t.Fatal("conflict response did not preserve disabled permission sync choice")
		}
	}
	if invalidator.allPermissions {
		t.Fatal("rejected transfer invalidated caches")
	}
	if group, ok, err := store.GetBroadcastGroup(ctx, groupName); err != nil || !ok || group.OwnerUserID != 0 {
		t.Fatalf("rejected handler transfer group=%+v ok=%v err=%v", group, ok, err)
	}
	if rec := post(hostSession, form(primaryBID, true)); rec.Code != http.StatusSeeOther {
		t.Fatalf("host transfer status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !invalidator.allPermissions {
		t.Fatal("successful transfer did not invalidate caches")
	}
	group, ok, err := store.GetBroadcastGroup(ctx, groupName)
	if err != nil || !ok || group.OwnerUserID != primaryBID || group.OwnerRemark != "河马" {
		t.Fatalf("transferred group=%+v ok=%v err=%v", group, ok, err)
	}
	if allowed, err := store.HasBroadcastPermissionScope(ctx, primaryAID, "group", 0, groupName); err != nil || !allowed {
		t.Fatalf("old owner permission lost allowed=%v err=%v", allowed, err)
	}
	permissions, err := store.ListBroadcastPermissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	foundAutoGrant := false
	for _, permission := range permissions {
		if permission.UserID == primaryBID && permission.Target == "chat" && permission.ChatID == chatID {
			foundAutoGrant = permission.GrantedBy == hostID
		}
	}
	if !foundAutoGrant {
		t.Fatalf("missing host-granted chat permission: %+v", permissions)
	}

	data, err := s.loadPageData(ctx, "", hostSession)
	if err != nil {
		t.Fatal(err)
	}
	primaryCandidates := map[int64]bool{}
	for _, op := range data.PrimaryOperators {
		primaryCandidates[op.UserID] = true
	}
	if !primaryCandidates[primaryAID] || !primaryCandidates[primaryBID] || primaryCandidates[disabledPrimaryID] || primaryCandidates[secondaryID] {
		t.Fatalf("owner candidates=%+v", data.PrimaryOperators)
	}
	var body strings.Builder
	if err := adminTemplate.Execute(&body, data); err != nil {
		t.Fatal(err)
	}
	html := body.String()
	for _, want := range []string{"创建者", "河马", "宿主", `data-owner-transfer-hook="broadcast-groups"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("rendered template missing %q", want)
		}
	}
	if strings.Contains(html, ">"+fmt.Sprint(primaryBID)+"<") {
		t.Fatalf("rendered owner display exposed full UID %d", primaryBID)
	}
}
