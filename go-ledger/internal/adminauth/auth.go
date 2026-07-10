package adminauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	RoleHost            = "host"
	RoleDefaultOperator = "default_operator"
	RoleOperator        = "operator"

	TicketTTL  = 5 * time.Minute
	SessionTTL = 7 * 24 * time.Hour
)

type Session struct {
	UserID    int64
	Role      string
	ExpiresAt time.Time
}

func NewToken() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data[:]), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

func SignSession(session Session, secret string) string {
	payload := fmt.Sprintf("%d|%s|%d", session.UserID, session.Role, session.ExpiresAt.Unix())
	encodedPayload := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signature := sessionSignature(encodedPayload, secret)
	return encodedPayload + "." + signature
}

func VerifySession(value, secret string, now time.Time) (Session, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Session{}, false
	}
	expected := sessionSignature(parts[0], secret)
	if !hmac.Equal([]byte(parts[1]), []byte(expected)) {
		return Session{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Session{}, false
	}
	fields := strings.Split(string(raw), "|")
	if len(fields) != 3 {
		return Session{}, false
	}
	userID, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || userID <= 0 {
		return Session{}, false
	}
	exp, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return Session{}, false
	}
	session := Session{
		UserID:    userID,
		Role:      fields[1],
		ExpiresAt: time.Unix(exp, 0),
	}
	if !IsAllowedRole(session.Role) || !session.ExpiresAt.After(now) {
		return Session{}, false
	}
	return session, true
}

func IsAllowedRole(role string) bool {
	switch role {
	case RoleHost, RoleDefaultOperator, RoleOperator:
		return true
	default:
		return false
	}
}

func IsHost(role string) bool {
	return role == RoleHost
}

func RoleLabel(role string) string {
	switch role {
	case RoleHost:
		return "宿主"
	case RoleDefaultOperator:
		return "默认操作人"
	case RoleOperator:
		return "操作人"
	default:
		return "未知"
	}
}

func sessionSignature(payload, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
