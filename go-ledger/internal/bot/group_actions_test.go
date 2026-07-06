package bot

import (
	"strings"
	"testing"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestMentionUserPrefersUsername(t *testing.T) {
	got := mentionUser(storage.User{ID: 123, Username: "aze89", DisplayName: "阿泽"})
	if got != "@aze89" {
		t.Fatalf("mentionUser = %q, want @aze89", got)
	}
}

func TestMentionUserFallsBackToUserLink(t *testing.T) {
	got := mentionUser(storage.User{ID: 123, DisplayName: "阿泽"})
	if !strings.Contains(got, `tg://user?id=123`) || !strings.Contains(got, "阿泽") {
		t.Fatalf("mentionUser fallback = %q", got)
	}
}
