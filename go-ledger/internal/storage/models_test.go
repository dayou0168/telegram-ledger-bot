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
