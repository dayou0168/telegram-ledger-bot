package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
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
