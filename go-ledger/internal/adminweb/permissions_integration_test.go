package adminweb

import (
	"context"
	"os"
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
