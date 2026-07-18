package adminweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/jackc/pgx/v5"
)

func TestPostgresAdminTicketIdentityAndFreshSessionAuthorization(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := int64(780000000000 + time.Now().UnixNano()%1000000)
	hostID := base
	primaryID := base + 1
	secondaryID := base + 2
	singleGroupOperatorID := base + 3
	chatID := -base
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := store.UpsertGlobalOperator(ctx, primaryID, "primary", 0, hostID, "primary", now); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertGlobalOperator(ctx, secondaryID, "secondary", primaryID, primaryID, "secondary", now); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureGroup(ctx, chatID, "single group", now); err != nil {
		t.Fatal(err)
	}
	if err := store.AddOperator(ctx, chatID, storage.User{ID: singleGroupOperatorID, DisplayName: "local"}, hostID, now); err != nil {
		t.Fatal(err)
	}

	const sessionSecret = "independent-session-secret"
	const secondFactor = "optional-second-factor"
	s := New(config.Config{
		HostUserID: hostID, AdminSessionSecret: sessionSecret, AdminWebToken: secondFactor,
	}, store)
	token := "ticket-identity-" + time.Now().Format("150405.000000000")
	tokenHash := adminauth.HashToken(token)
	if err := store.CreateAdminLoginTicket(ctx, tokenHash, secondaryID, now.Add(adminauth.TicketTTL), now); err != nil {
		t.Fatal(err)
	}
	forceAdminTicketCompatibilityRole(t, ctx, dsn, tokenHash, adminauth.RoleHost)

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/admin/login?ticket="+url.QueryEscape(token), nil).WithContext(ctx)
	s.renderTicketLogin(getRec, getReq, token)
	if !strings.Contains(getRec.Body.String(), `name="ticket"`) || !strings.Contains(getRec.Body.String(), `name="password"`) {
		t.Fatalf("ticket login form missing identity or second factor: %q", getRec.Body.String())
	}
	if _, ok, err := store.GetAdminLoginTicket(ctx, tokenHash, now); err != nil || !ok {
		t.Fatalf("GET consumed ticket ok=%v err=%v", ok, err)
	}

	postLogin := func(password string) *httptest.ResponseRecorder {
		form := url.Values{"ticket": {token}, "password": {password}}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		s.login(rec, req)
		return rec
	}
	wrongRec := postLogin("wrong")
	if len(wrongRec.Result().Cookies()) != 0 || !strings.Contains(wrongRec.Body.String(), "尚未消费") {
		t.Fatalf("wrong second factor response=%d body=%q", wrongRec.Code, wrongRec.Body.String())
	}
	if _, ok, err := store.GetAdminLoginTicket(ctx, tokenHash, now); err != nil || !ok {
		t.Fatalf("wrong password consumed ticket ok=%v err=%v", ok, err)
	}

	successRec := postLogin(secondFactor)
	if successRec.Code != http.StatusSeeOther {
		t.Fatalf("ticket login status=%d body=%q", successRec.Code, successRec.Body.String())
	}
	var sessionCookie *http.Cookie
	for _, cookie := range successRec.Result().Cookies() {
		if cookie.Name == adminCookieName && cookie.MaxAge > 0 {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("ticket login did not create session cookie")
	}
	session, ok := adminauth.VerifySession(sessionCookie.Value, sessionSecret, time.Now())
	if !ok || session.UserID != secondaryID || session.Role != adminauth.RoleOperator {
		t.Fatalf("session=%+v ok=%v", session, ok)
	}
	if _, ok := adminauth.VerifySession(sessionCookie.Value, secondFactor, time.Now()); ok {
		t.Fatal("ADMIN_WEB_TOKEN verified a cookie signed by ADMIN_SESSION_SECRET")
	}
	if _, ok, err := store.ConsumeAdminLoginTicket(ctx, tokenHash, time.Now()); err != nil || ok {
		t.Fatalf("ticket reused ok=%v err=%v", ok, err)
	}

	allowedHandler := s.withAuth(func(w http.ResponseWriter, r *http.Request) {
		fresh := adminSessionFromContext(r.Context())
		if fresh.UserID != secondaryID || fresh.Role != adminauth.RoleOperator {
			t.Fatalf("fresh session=%+v", fresh)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	requestWithCookie := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/admin", nil).WithContext(ctx)
		req.AddCookie(sessionCookie)
		allowedHandler(rec, req)
		return rec
	}
	if rec := requestWithCookie(); rec.Code != http.StatusNoContent {
		t.Fatalf("active session status=%d", rec.Code)
	}
	if disabled, err := store.DisableGlobalOperator(ctx, secondaryID, hostID, now.Add(time.Second)); err != nil || !disabled {
		t.Fatalf("disable secondary=%v err=%v", disabled, err)
	}
	if rec := requestWithCookie(); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/admin/login" {
		t.Fatalf("disabled session status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	localToken := "local-ticket-" + time.Now().Format("150405.000000000")
	if err := store.CreateAdminLoginTicket(ctx, adminauth.HashToken(localToken), singleGroupOperatorID, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	forceAdminTicketCompatibilityRole(t, ctx, dsn, adminauth.HashToken(localToken), adminauth.RoleHost)
	localForm := url.Values{"ticket": {localToken}, "password": {secondFactor}}
	localRec := httptest.NewRecorder()
	localReq := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(localForm.Encode())).WithContext(ctx)
	localReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.login(localRec, localReq)
	if len(localRec.Result().Cookies()) != 0 || !strings.Contains(localRec.Body.String(), "身份已失效") {
		t.Fatalf("single-group operator login status=%d body=%q", localRec.Code, localRec.Body.String())
	}
}

func forceAdminTicketCompatibilityRole(t *testing.T, ctx context.Context, dsn, tokenHash, role string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE admin_login_tickets SET role=$2 WHERE token_hash=$1`, tokenHash, role); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresMessageObserverHandlersAreHostOnly(t *testing.T) {
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

	base := int64(781000000000 + time.Now().UnixNano()%1000000)
	hostID, primaryAID, primaryBID, secondaryID := base, base+1, base+2, base+3
	now := time.Now().UTC()
	for _, op := range []struct {
		id, parent int64
		level      string
	}{
		{primaryAID, 0, "primary"},
		{primaryBID, 0, "primary"},
		{secondaryID, primaryAID, "secondary"},
	} {
		if err := store.UpsertGlobalOperator(ctx, op.id, op.level, op.parent, hostID, op.level, now); err != nil {
			t.Fatal(err)
		}
	}
	s := New(config.Config{HostUserID: hostID, AdminSessionSecret: "session"}, store)
	post := func(session adminauth.Session, handler http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/admin/message-observer", strings.NewReader(form.Encode())).WithContext(ctx)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(context.WithValue(req.Context(), adminContextKey{}, session))
		handler(rec, req)
		return rec
	}
	grantForm := url.Values{
		"source_secondary_user_id": {strconv.FormatInt(secondaryID, 10)},
		"observer_primary_user_id": {strconv.FormatInt(primaryBID, 10)},
		"receive_broadcast":        {"1"},
	}
	primarySession := adminauth.Session{UserID: primaryAID, Role: adminauth.RoleOperator}
	secondarySession := adminauth.Session{UserID: secondaryID, Role: adminauth.RoleOperator}
	forgedHostSession := adminauth.Session{UserID: hostID, Role: adminauth.RoleOperator}
	if rec := post(forgedHostSession, s.saveMessageObserver, grantForm); rec.Code != http.StatusForbidden {
		t.Fatalf("host uid with non-host role forged grant status=%d", rec.Code)
	}
	if rec := post(primarySession, s.saveMessageObserver, grantForm); rec.Code != http.StatusForbidden {
		t.Fatalf("primary forged grant status=%d", rec.Code)
	}
	if rec := post(secondarySession, s.saveMessageObserver, grantForm); rec.Code != http.StatusForbidden {
		t.Fatalf("secondary forged grant status=%d", rec.Code)
	}
	hostSession := adminauth.Session{UserID: hostID, Role: adminauth.RoleHost}
	if rec := post(hostSession, s.saveMessageObserver, grantForm); rec.Code != http.StatusSeeOther {
		t.Fatalf("host grant status=%d body=%q", rec.Code, rec.Body.String())
	}
	grants, err := store.ListOperatorMessageObserverGrants(ctx)
	if err != nil || !hasActiveMessageObserverGrant(grants, secondaryID, primaryBID) {
		t.Fatalf("host grant=%+v err=%v", grants, err)
	}
	if data, err := s.loadPageData(ctx, "", primarySession); err != nil || len(data.MessageObserverGrants) != 0 {
		t.Fatalf("primary observer page data=%+v err=%v", data.MessageObserverGrants, err)
	}
	if data, err := s.loadPageData(ctx, "", secondarySession); err != nil || len(data.MessageObserverGrants) != 0 {
		t.Fatalf("secondary observer page data=%+v err=%v", data.MessageObserverGrants, err)
	}
	if data, err := s.loadPageData(ctx, "", hostSession); err != nil ||
		!hasActiveMessageObserverGrant(data.MessageObserverGrants, secondaryID, primaryBID) {
		t.Fatalf("host observer page data=%+v err=%v", data.MessageObserverGrants, err)
	}
	if rec := post(primarySession, s.revokeMessageObserver, grantForm); rec.Code != http.StatusForbidden {
		t.Fatalf("primary forged revoke status=%d", rec.Code)
	}
	if rec := post(hostSession, s.revokeMessageObserver, grantForm); rec.Code != http.StatusSeeOther {
		t.Fatalf("host revoke status=%d body=%q", rec.Code, rec.Body.String())
	}
}

func hasActiveMessageObserverGrant(grants []storage.OperatorMessageObserverGrant, sourceUserID, observerUserID int64) bool {
	for _, grant := range grants {
		if grant.SourceSecondaryUserID == sourceUserID &&
			grant.ObserverPrimaryUserID == observerUserID && grant.Active {
			return true
		}
	}
	return false
}
