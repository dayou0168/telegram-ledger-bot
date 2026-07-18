package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestRetryAfterError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second)
	_, err := client.SendMessage(context.Background(), 1, "hello", nil)
	if err == nil {
		t.Fatal("expected telegram error")
	}
	delay, ok := RetryAfter(err)
	if !ok || delay != 3*time.Second {
		t.Fatalf("RetryAfter = %v, %v", delay, ok)
	}
}

func TestGetUpdatesRequestsChatMemberEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var allowed []string
		if err := json.Unmarshal([]byte(r.URL.Query().Get("allowed_updates")), &allowed); err != nil {
			t.Fatalf("decode allowed_updates: %v", err)
		}
		want := []string{"message", "callback_query", "my_chat_member", "chat_member"}
		if !reflect.DeepEqual(allowed, want) {
			t.Fatalf("allowed_updates=%v want=%v", allowed, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "token", time.Second)
	if _, err := client.GetUpdates(context.Background(), 1, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestSendPhotoUsesPersistentFileIDAndCaption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottoken/sendPhoto" {
			t.Fatalf("path=%q", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["photo"] != "telegram-file-id" || payload["caption"] != "caption" || payload["parse_mode"] != "HTML" {
			t.Fatalf("payload=%v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"chat":{"id":1,"type":"private"},"photo":[{"file_id":"telegram-file-id","width":100,"height":100}]}}`))
	}))
	defer server.Close()
	client := NewClient(server.URL, "token", time.Second)
	msg, err := client.SendPhoto(context.Background(), 1, "telegram-file-id", "caption", map[string]any{"parse_mode": "HTML"})
	if err != nil || msg.MessageID != 77 {
		t.Fatalf("message=%+v err=%v", msg, err)
	}
}
