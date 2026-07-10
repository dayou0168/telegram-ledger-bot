package adminauth

import (
	"testing"
	"time"
)

func TestSessionSignAndVerify(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	session := Session{UserID: 8453656635, Role: RoleOperator, ExpiresAt: now.Add(time.Hour)}
	value := SignSession(session, "secret")
	got, ok := VerifySession(value, "secret", now)
	if !ok {
		t.Fatal("session should verify")
	}
	if got.UserID != session.UserID || got.Role != session.Role || !got.ExpiresAt.Equal(session.ExpiresAt) {
		t.Fatalf("unexpected session: %+v", got)
	}
	if _, ok := VerifySession(value, "other-secret", now); ok {
		t.Fatal("session should not verify with a different secret")
	}
	if _, ok := VerifySession(value, "secret", now.Add(2*time.Hour)); ok {
		t.Fatal("expired session should not verify")
	}
}

func TestTokenHashIsStable(t *testing.T) {
	a := HashToken(" ticket ")
	b := HashToken("ticket")
	if a == "" || a != b {
		t.Fatalf("token hash should trim and be stable: %q %q", a, b)
	}
}
