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
		[]int64{secondaryID},
		[]int64{hostID, defaultID, primaryID, otherPrimaryID, otherSecondaryID, disabledSecondaryID})
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
