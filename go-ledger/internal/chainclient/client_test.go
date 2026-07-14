package chainclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainwatcher"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
)

func TestUpsertSubscriptionSendsCredentialsAndPayload(t *testing.T) {
	var gotPath string
	var gotBotID string
	var gotAuth string
	var got chainwatcher.SubscriptionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBotID = r.Header.Get("X-Bot-ID")
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer server.Close()

	client := New(server.URL, "bot-a", "secret-a", time.Second)
	err := client.UpsertSubscription(context.Background(), storage.WatchTarget{
		OwnerUserID:     88,
		Address:         "TAddress",
		Label:           "desk",
		WatchIncome:     true,
		WatchExpense:    false,
		NotifyTRX:       true,
		MinNotifyAmount: "10",
	})
	if err != nil {
		t.Fatalf("upsert subscription: %v", err)
	}
	if gotPath != "/v1/subscriptions/upsert" {
		t.Fatalf("unexpected path %q", gotPath)
	}
	if gotBotID != "bot-a" || gotAuth != "Bearer secret-a" {
		t.Fatalf("unexpected auth headers: bot=%q auth=%q", gotBotID, gotAuth)
	}
	if got.ChatID != 88 || got.OwnerUserID != 88 || got.Address != "TAddress" || got.MinNotifyAmount != "10" || !got.WatchIncome || got.WatchExpense {
		t.Fatalf("unexpected payload: %#v", got)
	}
}

func TestClaimEventsParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events/claim" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(chainwatcher.ClaimResponse{Events: []chainwatcher.MatchedEvent{{
			DeliveryID:   "d1",
			BotID:        "bot-a",
			OwnerUserID:  88,
			WatchAddress: "TTo",
			Direction:    "income",
			TxHash:       "hash",
		}}})
	}))
	defer server.Close()

	client := New(server.URL, "bot-a", "secret-a", time.Second)
	events, err := client.ClaimEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("claim events: %v", err)
	}
	if len(events) != 1 || events[0].DeliveryID != "d1" || events[0].Direction != "income" {
		t.Fatalf("unexpected events: %#v", events)
	}
}

func TestReadyReturnsDegradedError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(chainwatcher.StatusResponse{
			Status: "degraded/continuity", Ready: false, SourceReady: true, ContinuityReady: false,
		})
	}))
	defer server.Close()

	client := New(server.URL, "bot-a", "secret-a", time.Second)
	status, err := client.Ready(context.Background())
	if err == nil {
		t.Fatal("Ready() error = nil, want degraded error")
	}
	if status.Ready || !status.SourceReady || status.Status != "degraded/continuity" {
		t.Fatalf("unexpected status: %#v", status)
	}
}

func TestReadyAcceptsSuccessfulEmptySource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(chainwatcher.StatusResponse{Status: "ready", Ready: true})
	}))
	defer server.Close()

	client := New(server.URL, "bot-a", "secret-a", time.Second)
	status, err := client.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	if !status.Ready {
		t.Fatalf("Ready() status = %#v, want ready", status)
	}
}
