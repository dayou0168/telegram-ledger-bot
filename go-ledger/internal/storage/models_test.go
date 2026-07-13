package storage

import (
	"reflect"
	"strings"
	"testing"
)

func TestPrivateChatMessageDoesNotStoreContent(t *testing.T) {
	privateMessageType := reflect.TypeOf(PrivateChatMessage{})
	for i := 0; i < privateMessageType.NumField(); i++ {
		name := strings.ToLower(privateMessageType.Field(i).Name)
		for _, blocked := range []string{"text", "content", "caption", "file"} {
			if strings.Contains(name, blocked) {
				t.Fatalf("PrivateChatMessage should store metadata only, found field %q", privateMessageType.Field(i).Name)
			}
		}
	}
}

func TestPrivateCleanupScopesAreCanonicalAndExtensible(t *testing.T) {
	settings := NormalizePrivateCleanupSettings(PrivateCleanupSettings{
		Enabled: true,
		Scope:   "menu,broadcast,menu,unknown",
	})
	if settings.Scope != "broadcast,menu" {
		t.Fatalf("normalized scope = %q", settings.Scope)
	}
	if !PrivateCleanupScopeIncludes(settings.Scope, "broadcast") || !PrivateCleanupScopeIncludes(settings.Scope, "menu") {
		t.Fatal("selected scopes should be enabled")
	}
	if PrivateCleanupScopeIncludes(settings.Scope, "quick_reply") || PrivateCleanupScopeIncludes(settings.Scope, "private") {
		t.Fatal("unselected or legacy categories should not be enabled")
	}
}
