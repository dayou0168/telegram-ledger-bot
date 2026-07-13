package adminweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/ledgerperiod"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/xuri/excelize/v2"
)

const adminCookieName = "ledger_admin_token"

const adminListPageSize = 50

type adminContextKey struct{}

type Server struct {
	cfg         config.Config
	store       *storage.Store
	perms       permissions.Policy
	invalidator PermissionCacheInvalidator
	stats       SendGatewayStatsProvider
}

type PermissionCacheInvalidator interface {
	InvalidateBroadcastPermission(userID int64)
	InvalidateAllPermissionCaches()
	InvalidateWatchTargets()
}

type SendGatewayStatsProvider interface {
	TelegramSendGatewayStats() any
}

type pageData struct {
	Version                       string
	TokenUnset                    bool
	Message                       string
	Groups                        []storage.Group
	BGroups                       []storage.BroadcastGroup
	BroadcastMemberships          []storage.BroadcastGroup
	BOperators                    []storage.GlobalOperator
	PermissionOperators           []storage.GlobalOperator
	PrimaryOperators              []storage.GlobalOperator
	Permissions                   []storage.BroadcastPermission
	PermissionFilterData          []storage.BroadcastPermission
	Replace                       storage.BroadcastReplaceSetting
	WatchTargets                  []storage.WatchTarget
	AdminUserID                   int64
	AdminRole                     string
	AdminRoleLabel                string
	CanManageGlobal               bool
	CanManageOperators            bool
	CanManageBroadcastPermissions bool
	CanManageBroadcastGroups      bool
	ChatNames                     map[int64]string
	OpLabels                      map[int64]string
	GroupPager                    adminPager
	BroadcastPager                adminPager
	OperatorPager                 adminPager
	PermissionPager               adminPager
}

type adminPager struct {
	Query    string
	Page     int
	HasPrev  bool
	HasNext  bool
	PrevURL  string
	NextURL  string
	ItemFrom int
	ItemTo   int
	Total    int
}

type adminListFilters struct {
	GroupQuery      string
	GroupPage       int
	BroadcastQuery  string
	BroadcastPage   int
	OperatorQuery   string
	OperatorPage    int
	PermissionQuery string
	PermissionPage  int
}

type outboxStatusResponse struct {
	storage.NotificationOutboxStats
	QueueLength          int64  `json:"queue_length"`
	LastErrorHint        string `json:"last_error_hint"`
	StatsWindowHours     int64  `json:"stats_window_hours"`
	SentRetentionHours   int64  `json:"sent_retention_hours"`
	FailedRetentionHours int64  `json:"failed_retention_hours"`
	SendGateway          any    `json:"send_gateway,omitempty"`
}

type billData struct {
	Group         storage.Group
	DayKey        string
	TitleDay      string
	Summary       billSummary
	HistoryLinks  []billHistoryLink
	TodayPath     string
	PrevPath      string
	NextPath      string
	NewerPagePath string
	OlderPagePath string
	FilterSuffix  string
	DownloadPath  string
	Query         string
	Field         string
}

type billSummary struct {
	Deposits              []storage.Record
	Payouts               []storage.Record
	DepositCount          int
	PayoutCount           int
	TotalDepositCNY       string
	TotalDepositGrossUSDT string
	TotalDepositNetCNY    string
	TotalDepositNetUSDT   string
	TotalPayoutCNY        string
	TotalPayoutUSDT       string
	BalanceCNY            string
	BalanceUSDT           string
	CommissionCNY         string
	ExchangeRate          string
	FeeRate               string
	SubjectStats          []billPeopleStat
	ActorStats            []billPeopleStat
	RemarkStats           []billPeopleStat
	RateStats             []billRateStat
}

type billHistoryLink struct {
	DayKey string
	Label  string
	URL    string
	Active bool
}

type billPeopleStat struct {
	Name        string
	Count       int
	InCNY       string
	InUSDT      string
	OutCNY      string
	OutUSDT     string
	BalanceCNY  string
	BalanceUSDT string
}

type billRateStat struct {
	Rate       string
	AmountCNY  string
	AmountUSDT string
}

func New(cfg config.Config, store *storage.Store, invalidator ...PermissionCacheInvalidator) *Server {
	s := &Server{
		cfg:   cfg,
		store: store,
		perms: permissions.NewPolicy(cfg.HostUserID, cfg.DefaultOperatorIDs),
	}
	if len(invalidator) > 0 {
		s.invalidator = invalidator[0]
		if stats, ok := any(invalidator[0]).(SendGatewayStatsProvider); ok {
			s.stats = stats
		}
	}
	return s
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/b/", s.bill)
	mux.HandleFunc("/day_xxb.php", s.legacyBill)
	mux.HandleFunc("/admin/login", s.login)
	mux.HandleFunc("/admin/logout", s.logout)
	mux.HandleFunc("/admin", s.withAuth(s.index))
	mux.HandleFunc("/admin/group/save", s.withAuth(s.saveGroup))
	mux.HandleFunc("/admin/group/delete", s.withAuth(s.deleteGroup))
	mux.HandleFunc("/admin/group/rename", s.withAuth(s.renameGroup))
	mux.HandleFunc("/admin/group/add", s.withAuth(s.addGroupChats))
	mux.HandleFunc("/admin/group/remove", s.withAuth(s.removeGroupChats))
	mux.HandleFunc("/admin/operator/save", s.withAuth(s.saveOperator))
	mux.HandleFunc("/admin/operator/disable", s.withAuth(s.disableOperator))
	mux.HandleFunc("/admin/operator/cleanup", s.withAuth(s.saveOperatorCleanup))
	mux.HandleFunc("/admin/permission/grant", s.withAuth(s.grantPermission))
	mux.HandleFunc("/admin/permission/revoke", s.withAuth(s.revokePermission))
	mux.HandleFunc("/admin/watch/save", s.withAuth(s.saveWatchTarget))
	mux.HandleFunc("/admin/watch/remove", s.withAuth(s.removeWatchTarget))
	mux.HandleFunc("/admin/replace/save", s.withAuth(s.saveReplace))
	mux.HandleFunc("/admin/outbox/status", s.withAuth(s.outboxStatus))

	addr := fmt.Sprintf("%s:%d", s.cfg.AdminWebHost, s.cfg.AdminWebPort)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("admin web listening on %s", addr)
	err := server.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) outboxStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireGlobalAdmin(w, r) {
		return
	}
	statsWindow := s.cfg.OutboxStatsWindow
	if statsWindow <= 0 {
		statsWindow = 72 * time.Hour
	}
	sentRetention := s.cfg.OutboxSentRetention
	if sentRetention <= 0 {
		sentRetention = 72 * time.Hour
	}
	failedRetention := s.cfg.OutboxFailedRetention
	if failedRetention <= 0 {
		failedRetention = 14 * 24 * time.Hour
	}
	statsSince := time.Now().Add(-statsWindow)
	stats, err := s.store.NotificationOutboxStats(r.Context(), statsSince)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	response := outboxStatusResponse{
		NotificationOutboxStats: stats,
		QueueLength:             stats.Pending + stats.Sending,
		LastErrorHint:           outboxErrorHint(stats.LastError),
		StatsWindowHours:        int64(statsWindow / time.Hour),
		SentRetentionHours:      int64(sentRetention / time.Hour),
		FailedRetentionHours:    int64(failedRetention / time.Hour),
	}
	if s.stats != nil {
		response.SendGateway = s.stats.TelegramSendGatewayStats()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("encode outbox status: %v", err)
	}
}

func (s *Server) bill(w http.ResponseWriter, r *http.Request) {
	chatID, dayKey, action, ok := parseBillPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	group, err := s.store.GetGroup(r.Context(), chatID)
	if err != nil {
		http.Error(w, "账单不存在", http.StatusNotFound)
		return
	}
	query := billQueryText(r)
	field := billQueryField(r)
	filter := storage.RecordFilter{Field: field, Query: query}
	if action == "download" {
		s.downloadBill(w, r, group, dayKey, filter)
		return
	}
	beforeID := positiveInt64(r.URL.Query().Get("before"))
	afterID := positiveInt64(r.URL.Query().Get("after"))
	page, err := s.store.ListRecordsForDayPage(r.Context(), chatID, dayKey, filter, beforeID, afterID, 100)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	accumulator := newBillAccumulator(group, false)
	if err := s.store.WalkRecordsForDay(r.Context(), chatID, dayKey, filter, 500, func(records []storage.Record) error {
		for _, record := range records {
			accumulator.Add(record)
		}
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	days, err := s.store.ListBillDays(r.Context(), chatID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	summary := accumulator.Summary()
	for _, record := range page.Records {
		if record.Kind == "deposit" {
			summary.Deposits = append(summary.Deposits, record)
		} else if record.Kind == "payout" {
			summary.Payouts = append(summary.Payouts, record)
		}
	}
	var newerPagePath, olderPagePath string
	if len(page.Records) > 0 && page.HasNewer {
		newerPagePath = billCursorPath(chatID, dayKey, field, query, "after", page.Records[len(page.Records)-1].ID)
	}
	if len(page.Records) > 0 && page.HasOlder {
		olderPagePath = billCursorPath(chatID, dayKey, field, query, "before", page.Records[0].ID)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := billTemplate.Execute(w, billData{
		Group:         group,
		DayKey:        dayKey,
		TitleDay:      dayKey,
		Summary:       summary,
		HistoryLinks:  buildBillHistoryLinks(chatID, days, dayKey, field, query, 30),
		TodayPath:     billPath(chatID, s.currentBillDay(group)) + billFilterSuffix(field, query),
		PrevPath:      billPath(chatID, addDay(dayKey, -1)) + billFilterSuffix(field, query),
		NextPath:      billPath(chatID, addDay(dayKey, 1)) + billFilterSuffix(field, query),
		NewerPagePath: newerPagePath,
		OlderPagePath: olderPagePath,
		FilterSuffix:  billFilterSuffix(field, query),
		DownloadPath:  billDownloadPath(chatID, dayKey, field, query),
		Query:         query,
		Field:         field,
	}); err != nil {
		log.Printf("render bill: %v", err)
	}
}

func (s *Server) legacyBill(w http.ResponseWriter, r *http.Request) {
	values := r.URL.Query()
	chatID, err := strconv.ParseInt(strings.TrimSpace(values.Get("chat_id")), 10, 64)
	if err != nil || chatID == 0 {
		http.Error(w, "缺少 chat_id", http.StatusBadRequest)
		return
	}
	group, err := s.store.GetGroup(r.Context(), chatID)
	if err != nil {
		http.Error(w, "账单不存在", http.StatusNotFound)
		return
	}
	dayKey := normalizeBillDay(values.Get("created_at"))
	if dayKey == "" {
		dayKey = normalizeBillDay(datePart(values.Get("begintime")))
	}
	if dayKey == "" {
		dayKey = s.currentBillDay(group)
	}
	path := billPath(chatID, dayKey)
	if strings.TrimSpace(values.Get("download")) != "" {
		path += "/download"
	}
	query := strings.TrimSpace(values.Get("firstname"))
	field := legacyBillType(values.Get("type"))
	if suffix := billFilterSuffix(field, query); suffix != "" {
		path += suffix
	}
	http.Redirect(w, r, path, http.StatusFound)
}

func (s *Server) currentBillDay(group storage.Group) string {
	loc, err := time.LoadLocation(s.cfg.Timezone)
	if err != nil {
		loc = time.FixedZone("Asia/Shanghai", 8*3600)
	}
	now := time.Now().In(loc)
	return ledgerperiod.CurrentDayKey(group, now)
}

func (s *Server) downloadBill(w http.ResponseWriter, r *http.Request, group storage.Group, dayKey string, filter storage.RecordFilter) {
	fileName := fmt.Sprintf("账单_%s_%s.xlsx", dayKey, safeFileName(group.Title, "ledger"))
	fallback := fmt.Sprintf("ledger_%s.xlsx", strings.ReplaceAll(dayKey, "-", ""))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, url.PathEscape(fileName)))
	walker := func(kind string, visit func([]storage.Record) error) error {
		kindFilter := filter
		kindFilter.Kind = kind
		return s.store.WalkRecordsForDay(r.Context(), group.ChatID, dayKey, kindFilter, 500, visit)
	}
	if err := writeBillXLSX(group, dayKey, walker, w); err != nil {
		log.Printf("stream bill xlsx: %v", err)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if ticket := strings.TrimSpace(r.URL.Query().Get("ticket")); ticket != "" {
			s.renderTicketLogin(w, r, ticket)
			return
		}
		renderLogin(w, s.cfg.AdminWebToken == "", "")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if ticket := strings.TrimSpace(firstNonEmpty(r.FormValue("ticket"), r.URL.Query().Get("ticket"))); ticket != "" {
		s.loginWithTicket(w, r, ticket)
		return
	}
	if strings.TrimSpace(s.cfg.AdminWebToken) == "" {
		renderLogin(w, true, "未配置 ADMIN_WEB_TOKEN，无法创建后台会话")
		return
	}
	if s.cfg.AdminWebToken != "" && r.FormValue("password") != s.cfg.AdminWebToken {
		renderLogin(w, false, "密码不正确")
		return
	}
	hostUserID := s.perms.HostUserID()
	if hostUserID <= 0 {
		renderLogin(w, s.cfg.AdminWebToken == "", "未配置宿主 UID，无法创建后台会话")
		return
	}
	s.setAdminCookie(w, adminauth.Session{
		UserID:    hostUserID,
		Role:      adminauth.RoleHost,
		ExpiresAt: time.Now().Add(adminauth.SessionTTL),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) renderTicketLogin(w http.ResponseWriter, r *http.Request, ticket string) {
	now := time.Now()
	item, ok, err := s.store.GetAdminLoginTicket(r.Context(), adminauth.HashToken(ticket), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok || !adminauth.IsAllowedRole(item.Role) {
		renderLogin(w, s.cfg.AdminWebToken == "", "快捷登录链接无效或已过期，请输入后台密码登录")
		return
	}
	if ok, err := s.adminSessionAllowed(r.Context(), adminauth.Session{UserID: item.UserID, Role: item.Role, ExpiresAt: item.ExpiresAt}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		renderLogin(w, s.cfg.AdminWebToken == "", "后台登录身份已失效，请输入后台密码登录")
		return
	}
	renderLoginWithTicket(w, s.cfg.AdminWebToken == "", "快捷登录链接有效，点击下方按钮进入后台。", ticket)
}

func (s *Server) loginWithTicket(w http.ResponseWriter, r *http.Request, ticket string) {
	now := time.Now()
	if strings.TrimSpace(s.cfg.AdminWebToken) == "" {
		renderLogin(w, true, "未配置 ADMIN_WEB_TOKEN，无法创建后台会话")
		return
	}
	item, ok, err := s.store.ConsumeAdminLoginTicket(r.Context(), adminauth.HashToken(ticket), now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok || !adminauth.IsAllowedRole(item.Role) {
		renderLogin(w, s.cfg.AdminWebToken == "", "快捷登录链接无效或已过期，请输入后台密码登录")
		return
	}
	if ok, err := s.adminSessionAllowed(r.Context(), adminauth.Session{UserID: item.UserID, Role: item.Role, ExpiresAt: item.ExpiresAt}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		renderLogin(w, s.cfg.AdminWebToken == "", "后台登录身份已失效")
		return
	}
	_ = s.store.CleanupAdminLoginTickets(r.Context(), now.Add(-24*time.Hour))
	s.setAdminCookie(w, adminauth.Session{
		UserID:    item.UserID,
		Role:      item.Role,
		ExpiresAt: now.Add(adminauth.SessionTTL),
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Path: "/admin", MaxAge: -1})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if session, ok := s.adminSessionFromRequest(r); ok {
			allowed, err := s.adminSessionAllowed(r.Context(), session)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.SetCookie(w, &http.Cookie{Name: adminCookieName, Path: "/admin", MaxAge: -1})
				http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
				return
			}
			ctx := context.WithValue(r.Context(), adminContextKey{}, session)
			next(w, r.WithContext(ctx))
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}

func (s *Server) adminSessionFromRequest(r *http.Request) (adminauth.Session, bool) {
	if strings.TrimSpace(s.adminSessionSecret()) == "" {
		return adminauth.Session{}, false
	}
	cookie, err := r.Cookie(adminCookieName)
	if err != nil {
		return adminauth.Session{}, false
	}
	return adminauth.VerifySession(cookie.Value, s.adminSessionSecret(), time.Now())
}

func (s *Server) adminSessionAllowed(ctx context.Context, session adminauth.Session) (bool, error) {
	switch session.Role {
	case adminauth.RoleHost:
		return s.perms.IsHost(session.UserID), nil
	case adminauth.RoleDefaultOperator:
		return s.perms.IsDefaultOperator(session.UserID), nil
	case adminauth.RoleOperator:
		if session.UserID <= 0 {
			return false, nil
		}
		level, ok, err := s.store.GetGlobalOperatorLevel(ctx, session.UserID)
		if err != nil || !ok {
			return false, err
		}
		return s.perms.CanUsePrivateGlobalFeatures(session.UserID, permissions.UserCapabilities{
			GlobalOperatorLevel: level,
		}), nil
	default:
		return false, nil
	}
}

func (s *Server) setAdminCookie(w http.ResponseWriter, session adminauth.Session) {
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    adminauth.SignSession(session, s.adminSessionSecret()),
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   s.cfg.AdminWebCookieSecure,
		MaxAge:   int(time.Until(session.ExpiresAt).Seconds()),
	})
}

func (s *Server) adminSessionSecret() string {
	return strings.TrimSpace(s.cfg.AdminWebToken)
}

func adminSessionFromContext(ctx context.Context) adminauth.Session {
	if session, ok := ctx.Value(adminContextKey{}).(adminauth.Session); ok {
		return session
	}
	return adminauth.Session{}
}

func (s *Server) sessionCapabilities(ctx context.Context, session adminauth.Session) (permissions.UserCapabilities, error) {
	if session.Role != adminauth.RoleOperator {
		return permissions.UserCapabilities{}, nil
	}
	op, ok, err := s.store.GetGlobalOperator(ctx, session.UserID)
	if err != nil || !ok || op.Status != "active" {
		return permissions.UserCapabilities{}, err
	}
	return permissions.UserCapabilities{
		GlobalOperatorLevel: op.Level,
		ParentUserID:        op.ParentUserID,
	}, nil
}

func (s *Server) adminCanManageGlobal(session adminauth.Session) bool {
	return adminauth.IsHost(session.Role) && s.perms.CanManageGlobalAdmin(session.UserID)
}

func (s *Server) requireGlobalAdmin(w http.ResponseWriter, r *http.Request) bool {
	if s.adminCanManageGlobal(adminSessionFromContext(r.Context())) {
		return true
	}
	http.Error(w, "没有后台管理权限", http.StatusForbidden)
	return false
}

func requirePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func (s *Server) invalidateBroadcastPermission(userID int64) {
	if s.invalidator != nil {
		s.invalidator.InvalidateBroadcastPermission(userID)
	}
}

func (s *Server) invalidateAllPermissionCaches() {
	if s.invalidator != nil {
		s.invalidator.InvalidateAllPermissionCaches()
	}
}

func (s *Server) invalidateWatchTargets() {
	if s.invalidator != nil {
		s.invalidator.InvalidateWatchTargets()
	}
}

func parsePositiveFormID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	return id, err == nil && id > 0
}

func parseBroadcastPermissionForm(r *http.Request) (int64, string, int64, string, string, bool) {
	userID, ok := parsePositiveFormID(r, "user_id")
	if !ok {
		return 0, "", 0, "", "操作人 UID 不正确", false
	}
	target := strings.TrimSpace(r.FormValue("target"))
	switch target {
	case "chat":
		chatID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("chat_id")), 10, 64)
		if err != nil || chatID == 0 {
			return 0, "", 0, "", "单群目标不正确", false
		}
		return userID, target, chatID, "", "", true
	case "group":
		groupName := strings.TrimSpace(r.FormValue("group_name"))
		if groupName == "" {
			return 0, "", 0, "", "分组目标不能为空", false
		}
		return userID, target, 0, groupName, "", true
	default:
		return 0, "", 0, "", "权限类型不正确", false
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	filters := parseAdminListFilters(r)
	data, err := s.loadPageData(r.Context(), r.URL.Query().Get("msg"), adminSessionFromContext(r.Context()), filters)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTemplate.Execute(w, data); err != nil {
		log.Printf("render admin: %v", err)
	}
}

func (s *Server) saveGroup(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	session := adminSessionFromContext(r.Context())
	caps, err := s.operatorActorCapabilities(r.Context(), session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.perms.CanManageBroadcastGroups(session.UserID, caps) {
		http.Error(w, "没有广播分组管理权限", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectMsg(w, r, "分组名不能为空")
		return
	}
	ownerUserID := int64(0)
	if caps.GlobalOperatorLevel == "primary" {
		ownerUserID = session.UserID
	}
	created, err := s.store.CreateBroadcastGroup(r.Context(), name, session.UserID, ownerUserID, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !created {
		redirectMsg(w, r, "分组名已存在")
		return
	}
	s.invalidateAllPermissionCaches()
	redirectMsg(w, r, "分组已保存")
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectMsg(w, r, "分组名不能为空")
		return
	}
	session := adminSessionFromContext(r.Context())
	caps, err := s.operatorActorCapabilities(r.Context(), session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.perms.CanManageBroadcastGroups(session.UserID, caps) {
		http.Error(w, "没有广播分组管理权限", http.StatusForbidden)
		return
	}
	ok, _, err := s.store.DeleteBroadcastGroupManaged(r.Context(), name, session.UserID, s.perms.IsHost(session.UserID), time.Now())
	if errors.Is(err, storage.ErrBroadcastScopeDenied) {
		http.Error(w, "不能删除其他一级操作人的分组", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		redirectMsg(w, r, "分组不存在")
		return
	}
	s.invalidateAllPermissionCaches()
	redirectMsg(w, r, "分组已删除")
}

func (s *Server) renameGroup(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	oldName := strings.TrimSpace(r.FormValue("old_name"))
	newName := strings.TrimSpace(r.FormValue("new_name"))
	if oldName == "" || newName == "" {
		redirectMsg(w, r, "原分组名和新分组名不能为空")
		return
	}
	session := adminSessionFromContext(r.Context())
	caps, err := s.operatorActorCapabilities(r.Context(), session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.perms.CanManageBroadcastGroups(session.UserID, caps) {
		http.Error(w, "没有广播分组管理权限", http.StatusForbidden)
		return
	}
	ok, _, err := s.store.RenameBroadcastGroup(r.Context(), oldName, newName, session.UserID, s.perms.IsHost(session.UserID), time.Now())
	if errors.Is(err, storage.ErrBroadcastScopeDenied) {
		http.Error(w, "不能修改其他一级操作人的分组", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		redirectMsg(w, r, "原分组不存在")
		return
	}
	s.invalidateAllPermissionCaches()
	redirectMsg(w, r, "分组已改名，成员和授权已迁移")
}

func (s *Server) addGroupChats(w http.ResponseWriter, r *http.Request) {
	s.changeGroupChats(w, r, true)
}

func (s *Server) removeGroupChats(w http.ResponseWriter, r *http.Request) {
	s.changeGroupChats(w, r, false)
}

func (s *Server) changeGroupChats(w http.ResponseWriter, r *http.Request, add bool) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		redirectMsg(w, r, "分组名不能为空")
		return
	}
	chatIDs := parseIDList(r.Form["chat_id"])
	if len(chatIDs) == 0 {
		redirectMsg(w, r, "请选择群")
		return
	}
	session := adminSessionFromContext(r.Context())
	caps, err := s.operatorActorCapabilities(r.Context(), session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.perms.CanManageBroadcastGroups(session.UserID, caps) {
		http.Error(w, "没有广播分组管理权限", http.StatusForbidden)
		return
	}
	var count int
	manageAll := s.perms.IsHost(session.UserID)
	if add {
		count, err = s.store.AddChatsToBroadcastGroupManaged(r.Context(), name, chatIDs, session.UserID, manageAll, time.Now())
	} else {
		count, err = s.store.RemoveChatsFromBroadcastGroupManaged(r.Context(), name, chatIDs, session.UserID, manageAll, time.Now())
	}
	if errors.Is(err, storage.ErrBroadcastScopeDenied) {
		http.Error(w, "不能管理其他一级操作人的分组或未授权群", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if count > 0 {
		s.invalidateAllPermissionCaches()
	}
	action := "添加"
	if !add {
		action = "移除"
	}
	redirectMsg(w, r, fmt.Sprintf("已%s %d 个群", action, count))
}

func (s *Server) operatorActorCapabilities(ctx context.Context, session adminauth.Session) (permissions.UserCapabilities, error) {
	if session.Role == adminauth.RoleHost || session.Role == adminauth.RoleDefaultOperator {
		return permissions.UserCapabilities{}, nil
	}
	return s.sessionCapabilities(ctx, session)
}

func (s *Server) canMutateBroadcastPermission(ctx context.Context, session adminauth.Session, subjectUserID int64, target string, chatID int64, groupName string, granting bool) (bool, error) {
	caps, err := s.operatorActorCapabilities(ctx, session)
	if err != nil || !s.perms.CanManageBroadcastPermissions(session.UserID, caps) {
		return false, err
	}
	if s.perms.IsPrivileged(subjectUserID) {
		return false, nil
	}
	subject, ok, err := s.store.GetGlobalOperator(ctx, subjectUserID)
	if err != nil || !ok || subject.Status != "active" {
		return false, err
	}
	if s.perms.CanManageAllBroadcastPermissions(session.UserID) {
		return true, nil
	}
	if !s.perms.CanDelegateBroadcastPermission(session.UserID, caps, subject.UserID, subject.Level, subject.ParentUserID) {
		return false, nil
	}
	if granting {
		return s.store.HasDelegableBroadcastPermissionScope(ctx, session.UserID, target, chatID, groupName)
	}
	return s.store.HasBroadcastPermissionGrantedBy(ctx, subjectUserID, target, chatID, groupName, session.UserID)
}

func (s *Server) saveOperator(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, ok := parsePositiveFormID(r, "user_id")
	if !ok {
		redirectMsg(w, r, "操作人 UID 不正确")
		return
	}
	level := strings.TrimSpace(r.FormValue("level"))
	if level == "" {
		level = "secondary"
		if s.perms.IsHost(adminSessionFromContext(r.Context()).UserID) {
			level = "primary"
		}
	}
	if level != "primary" && level != "secondary" {
		redirectMsg(w, r, "操作人级别不正确")
		return
	}
	session := adminSessionFromContext(r.Context())
	caps, err := s.operatorActorCapabilities(r.Context(), session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !s.perms.CanCreateGlobalOperator(session.UserID, caps, level) || s.perms.IsPrivileged(userID) {
		http.Error(w, "no permission to grant global operator", http.StatusForbidden)
		return
	}
	parentUserID := int64(0)
	if level == "secondary" {
		if s.perms.IsHost(session.UserID) {
			var parentOK bool
			parentUserID, parentOK = parsePositiveFormID(r, "parent_user_id")
			if !parentOK {
				redirectMsg(w, r, "secondary operator requires a primary parent")
				return
			}
			parent, exists, err := s.store.GetGlobalOperator(r.Context(), parentUserID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if !exists || parent.Level != "primary" || parent.Status != "active" || s.perms.IsPrivileged(parentUserID) {
				http.Error(w, "secondary operator parent must be an active database primary", http.StatusForbidden)
				return
			}
		} else {
			parentUserID = session.UserID
		}
	}
	if existing, exists, err := s.store.GetGlobalOperator(r.Context(), userID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if exists {
		if existing.Level != level || existing.ParentUserID != parentUserID {
			http.Error(w, "global operator level and parent cannot be changed from this form", http.StatusForbidden)
			return
		}
	}
	if err := s.store.UpsertGlobalOperator(r.Context(), userID, level, parentUserID, session.UserID, r.FormValue("remark"), time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.invalidateAllPermissionCaches()
	redirectMsg(w, r, "操作人已保存")
}

func (s *Server) disableOperator(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, ok := parsePositiveFormID(r, "user_id")
	if !ok {
		redirectMsg(w, r, "操作人 UID 不正确")
		return
	}
	session := adminSessionFromContext(r.Context())
	caps, err := s.operatorActorCapabilities(r.Context(), session)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	target, exists, err := s.store.GetGlobalOperator(r.Context(), userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists || s.perms.IsPrivileged(userID) || !s.perms.CanDisableGlobalOperator(session.UserID, caps, target.Level, target.ParentUserID) {
		http.Error(w, "no permission to disable global operator", http.StatusForbidden)
		return
	}
	_, err = s.store.DisableGlobalOperator(r.Context(), userID, session.UserID, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.invalidateAllPermissionCaches()
	redirectMsg(w, r, "操作人已禁用")
}

func (s *Server) saveOperatorCleanup(w http.ResponseWriter, r *http.Request) {
	if !s.requireGlobalAdmin(w, r) {
		return
	}
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("user_id")), 10, 64)
	if err != nil || userID <= 0 {
		redirectMsg(w, r, "操作人 UID 不正确")
		return
	}
	enabled := r.FormValue("enabled") == "1"
	cleanupTime, ok := normalizeCleanupTimeParts(r.FormValue("cleanup_hour"), r.FormValue("cleanup_minute"))
	if !ok {
		redirectMsg(w, r, "每日兜底时间必须同时填写小时和分钟，小时为 0-23，分钟为 0-59")
		return
	}
	botAfter, ok := normalizeCleanupDelay(r.FormValue("bot_delete_after"), r.FormValue("bot_delete_after_custom"), r.FormValue("bot_delete_after_unit"))
	if enabled && !ok {
		redirectMsg(w, r, "机器人提示消息删除时间不正确")
		return
	}
	incomingEnabled := r.FormValue("incoming_enabled") == "1"
	incomingAfter, ok := normalizeCleanupDelay(r.FormValue("incoming_delete_after"), r.FormValue("incoming_delete_after_custom"), r.FormValue("incoming_delete_after_unit"))
	if enabled && incomingEnabled && !ok {
		redirectMsg(w, r, "用户临时消息删除时间不正确")
		return
	}
	if enabled && cleanupTime == "" && botAfter == 0 && (!incomingEnabled || incomingAfter == 0) {
		redirectMsg(w, r, "请至少选择一种私聊清理方式")
		return
	}
	if !enabled {
		cleanupTime = ""
		botAfter = 0
		incomingEnabled = false
		incomingAfter = 0
	}
	if incomingEnabled && incomingAfter == 0 && cleanupTime == "" {
		redirectMsg(w, r, "清理用户临时消息时，请选择发送后删除时间或每日兜底时间")
		return
	}
	scope := strings.Join(storage.PrivateCleanupScopes(strings.Join(r.Form["scope"], ",")), ",")
	if enabled && len(r.Form["scope"]) == 0 {
		redirectMsg(w, r, "请至少选择一个清理范围")
		return
	}
	if !s.perms.IsPrivileged(userID) {
		if ok, err := s.store.IsGlobalOperator(r.Context(), userID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		} else if !ok {
			redirectMsg(w, r, "操作人不存在或已禁用")
			return
		}
	} else if err := s.store.EnsurePrivateCleanupCarrier(r.Context(), userID, adminSessionFromContext(r.Context()).UserID, "", time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	now := time.Now()
	runDate := ""
	if enabled && cleanupTime != "" {
		localNow := now.In(privateCleanupLocation())
		if minutes, _ := storage.CleanupTimeMinutes(cleanupTime); minutes <= localNow.Hour()*60+localNow.Minute() {
			runDate = localNow.Format("2006-01-02")
		}
	}
	saved, err := s.store.SetBroadcastOperatorPrivateCleanupSettings(r.Context(), userID, storage.PrivateCleanupSettings{
		Enabled:             enabled,
		DailyTime:           cleanupTime,
		DailyLastRunDate:    runDate,
		BotDeleteAfter:      botAfter,
		IncomingEnabled:     incomingEnabled,
		IncomingDeleteAfter: incomingAfter,
		Scope:               scope,
	}, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !saved {
		redirectMsg(w, r, "操作人不存在")
		return
	}
	if enabled {
		redirectMsg(w, r, "私聊自动清空已设置")
		return
	}
	redirectMsg(w, r, "私聊自动清空已关闭")
}

func (s *Server) grantPermission(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, target, chatID, groupName, message, ok := parseBroadcastPermissionForm(r)
	if !ok {
		redirectMsg(w, r, message)
		return
	}
	session := adminSessionFromContext(r.Context())
	allowed, err := s.canMutateBroadcastPermission(r.Context(), session, userID, target, chatID, groupName, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Error(w, "broadcast grant exceeds actor scope", http.StatusForbidden)
		return
	}
	if ok, err := s.store.IsGlobalOperator(r.Context(), userID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if !ok {
		redirectMsg(w, r, "操作人不存在或已禁用")
		return
	}
	_, err = s.store.GrantBroadcastPermissionAuthorized(r.Context(), userID, target, chatID, groupName,
		session.UserID, s.perms.CanManageAllBroadcastPermissions(session.UserID), time.Now())
	if errors.Is(err, storage.ErrBroadcastScopeDenied) {
		http.Error(w, "broadcast grant exceeds actor scope", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.invalidateBroadcastPermission(userID)
	redirectMsg(w, r, "权限已授权")
}

func (s *Server) revokePermission(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, target, chatID, groupName, message, ok := parseBroadcastPermissionForm(r)
	if !ok {
		redirectMsg(w, r, message)
		return
	}
	session := adminSessionFromContext(r.Context())
	allowed, err := s.canMutateBroadcastPermission(r.Context(), session, userID, target, chatID, groupName, false)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !allowed {
		http.Error(w, "no permission to revoke broadcast scope", http.StatusForbidden)
		return
	}
	result, err := s.store.RevokeBroadcastPermissionAuthorized(r.Context(), userID, target, chatID, groupName,
		session.UserID, s.perms.CanManageAllBroadcastPermissions(session.UserID), time.Now())
	if errors.Is(err, storage.ErrBroadcastScopeDenied) {
		http.Error(w, "no permission to revoke broadcast scope", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if result.GroupMembershipsChanged {
		s.invalidateAllPermissionCaches()
	} else {
		s.invalidateBroadcastPermission(userID)
	}
	redirectMsg(w, r, "权限已取消")
}

func (s *Server) saveWatchTarget(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	session := adminSessionFromContext(r.Context())
	owner, address, ok := s.watchFormIdentity(w, r, session)
	if !ok {
		return
	}
	target, exists, err := s.store.GetWatchTarget(r.Context(), owner, address)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !exists {
		redirectMsg(w, r, "监听地址不存在")
		return
	}
	target.Label = strings.TrimSpace(r.FormValue("label"))
	target.WatchIncome = r.FormValue("watch_income") == "1"
	target.WatchExpense = r.FormValue("watch_expense") == "1"
	target.NotifyTRX = false
	target.MinNotifyAmount = normalizeAdminMinAmount(r.FormValue("min_notify_amount"))
	if _, err := s.store.UpdateWatchTarget(r.Context(), target, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.invalidateWatchTargets()
	redirectMsg(w, r, "监听地址已保存")
}

func (s *Server) removeWatchTarget(w http.ResponseWriter, r *http.Request) {
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	session := adminSessionFromContext(r.Context())
	owner, address, ok := s.watchFormIdentity(w, r, session)
	if !ok {
		return
	}
	removed, err := s.store.RemoveWatch(r.Context(), owner, address, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !removed {
		redirectMsg(w, r, "监听地址不存在")
		return
	}
	s.invalidateWatchTargets()
	redirectMsg(w, r, "监听地址已删除")
}

func (s *Server) watchFormIdentity(w http.ResponseWriter, r *http.Request, session adminauth.Session) (int64, string, bool) {
	owner, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("owner_user_id")), 10, 64)
	address := strings.TrimSpace(r.FormValue("address"))
	if owner <= 0 || address == "" {
		redirectMsg(w, r, "监听地址参数不完整")
		return 0, "", false
	}
	if !adminauth.IsHost(session.Role) && owner != session.UserID {
		http.Error(w, "没有权限操作其他人的监听地址", http.StatusForbidden)
		return 0, "", false
	}
	return owner, address, true
}

func (s *Server) saveReplace(w http.ResponseWriter, r *http.Request) {
	if !s.requireGlobalAdmin(w, r) {
		return
	}
	if !requirePost(w, r) {
		return
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	setting, err := s.store.GetBroadcastReplaceSetting(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setting.Enabled = r.FormValue("enabled") == "1"
	setting.Text = strings.TrimSpace(r.FormValue("text"))
	if r.FormValue("remove_image") == "1" {
		setting.ImageName = ""
		setting.ImageData = nil
	}
	file, header, err := r.FormFile("image")
	if err == nil {
		defer file.Close()
		data, readErr := io.ReadAll(io.LimitReader(file, 8<<20))
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		if len(data) > 0 {
			setting.ImageName = safeFileName(header.Filename, "replace.jpg")
			setting.ImageData = data
		}
	}
	setting.UpdatedAt = time.Now()
	if err := s.store.SaveBroadcastReplaceSetting(r.Context(), setting); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectMsg(w, r, "广播替换设置已保存")
}

func (s *Server) loadPageData(ctx context.Context, message string, session adminauth.Session, filterArgs ...adminListFilters) (pageData, error) {
	var filters adminListFilters
	if len(filterArgs) > 0 {
		filters = filterArgs[0]
	}
	caps, err := s.operatorActorCapabilities(ctx, session)
	if err != nil {
		return pageData{}, err
	}
	canManageGlobal := s.adminCanManageGlobal(session)
	canManageOperators := s.perms.CanCreateGlobalOperator(session.UserID, caps, "primary") ||
		s.perms.CanCreateGlobalOperator(session.UserID, caps, "secondary")
	canManageBroadcastPermissions := s.perms.CanManageBroadcastPermissions(session.UserID, caps)
	canManageBroadcastGroups := s.perms.CanManageBroadcastGroups(session.UserID, caps)
	var (
		groups              []storage.Group
		bgroups             []storage.BroadcastGroup
		operators           []storage.GlobalOperator
		allOperators        []storage.GlobalOperator
		permissionOperators []storage.GlobalOperator
		permissions         []storage.BroadcastPermission
		replace             storage.BroadcastReplaceSetting
	)
	if canManageGlobal || canManageBroadcastPermissions || canManageBroadcastGroups {
		allOperators, err = s.store.ListGlobalOperators(ctx)
		if err != nil {
			return pageData{}, err
		}
		filtered := allOperators[:0]
		for _, op := range allOperators {
			if !s.perms.IsPrivileged(op.UserID) {
				filtered = append(filtered, op)
			}
		}
		allOperators = filtered
	}
	if s.perms.CanManageAllBroadcastPermissions(session.UserID) {
		groups, err = s.store.ListGroups(ctx)
		if err != nil {
			return pageData{}, err
		}
		bgroups, err = s.store.ListBroadcastGroups(ctx)
		if err != nil {
			return pageData{}, err
		}
		permissions, err = s.store.ListBroadcastPermissions(ctx)
		if err != nil {
			return pageData{}, err
		}
		operators = append(operators, allOperators...)
		for _, op := range allOperators {
			if op.Status == "active" && (op.Level == "primary" || op.Level == "secondary") {
				permissionOperators = append(permissionOperators, op)
			}
		}
	} else if caps.GlobalOperatorLevel == "primary" {
		groups, err = s.store.ListDirectBroadcastChats(ctx, session.UserID)
		if err != nil {
			return pageData{}, err
		}
		bgroups, err = s.store.ListVisibleBroadcastGroups(ctx, session.UserID)
		if err != nil {
			return pageData{}, err
		}
		permissions, err = s.store.ListBroadcastPermissionsRelevantTo(ctx, session.UserID)
		if err != nil {
			return pageData{}, err
		}
		for _, op := range allOperators {
			if op.Level == "secondary" && op.ParentUserID == session.UserID {
				operators = append(operators, op)
			}
			if op.Status == "active" && ((op.Level == "secondary" && op.ParentUserID == session.UserID) ||
				(op.Level == "primary" && op.UserID != session.UserID)) {
				permissionOperators = append(permissionOperators, op)
			}
		}
	}
	primaryOperators := make([]storage.GlobalOperator, 0, len(operators))
	for _, op := range operators {
		if op.Status == "active" && op.Level == "primary" {
			primaryOperators = append(primaryOperators, op)
		}
	}
	if canManageGlobal {
		replace, err = s.store.GetBroadcastReplaceSetting(ctx)
		if err != nil {
			return pageData{}, err
		}
	}
	if canManageGlobal {
		configuredIDs := map[int64]struct{}{}
		for _, op := range operators {
			configuredIDs[op.UserID] = struct{}{}
		}
		var configOperators []storage.GlobalOperator
		hostUserID := s.perms.HostUserID()
		if hostUserID > 0 {
			if op, err := s.privateCleanupConfigOperator(ctx, hostUserID, "host", "宿主"); err != nil {
				return pageData{}, err
			} else {
				configOperators = append(configOperators, op)
			}
		}
		privilegedIDs := s.perms.PrivilegedUserIDs()
		defaultIDs := make([]int64, 0, len(privilegedIDs))
		for _, userID := range privilegedIDs {
			if userID > 0 && userID != hostUserID {
				defaultIDs = append(defaultIDs, userID)
			}
		}
		sort.Slice(defaultIDs, func(i, j int) bool { return defaultIDs[i] < defaultIDs[j] })
		for _, userID := range defaultIDs {
			if _, exists := configuredIDs[userID]; exists {
				continue
			}
			op, err := s.privateCleanupConfigOperator(ctx, userID, "default", "默认操作人")
			if err != nil {
				return pageData{}, err
			}
			configOperators = append(configOperators, op)
		}
		operators = append(configOperators, operators...)
	}
	var watchTargets []storage.WatchTarget
	if s.perms.IsHost(session.UserID) {
		watchTargets, err = s.store.ListWatchTargets(ctx)
	} else {
		watchTargets, err = s.store.ListWatchTargetsForOwner(ctx, session.UserID)
	}
	if err != nil {
		return pageData{}, err
	}
	chatNames := make(map[int64]string, len(groups))
	for _, group := range groups {
		chatNames[group.ChatID] = group.Title
	}
	opLabels := make(map[int64]string, len(allOperators)+len(operators))
	for _, op := range allOperators {
		opLabels[op.UserID] = operatorLabel(op)
	}
	for _, op := range operators {
		opLabels[op.UserID] = operatorLabel(op)
	}
	allBroadcastGroups := append([]storage.BroadcastGroup(nil), bgroups...)
	allPermissions := append([]storage.BroadcastPermission(nil), permissions...)
	groups, groupPager := pageAdminGroups(groups, filters)
	bgroups, broadcastPager := pageAdminBroadcastGroups(bgroups, filters)
	operators, operatorPager := pageAdminOperators(operators, filters)
	permissions, permissionPager := pageAdminPermissions(permissions, chatNames, opLabels, filters)
	return pageData{
		Version:                       config.Version,
		TokenUnset:                    s.cfg.AdminWebToken == "",
		Message:                       message,
		Groups:                        groups,
		BGroups:                       bgroups,
		BroadcastMemberships:          allBroadcastGroups,
		BOperators:                    operators,
		PermissionOperators:           permissionOperators,
		PrimaryOperators:              primaryOperators,
		Permissions:                   permissions,
		PermissionFilterData:          allPermissions,
		Replace:                       replace,
		WatchTargets:                  watchTargets,
		AdminUserID:                   session.UserID,
		AdminRole:                     session.Role,
		AdminRoleLabel:                adminauth.RoleLabel(session.Role),
		CanManageGlobal:               canManageGlobal,
		CanManageOperators:            canManageOperators,
		CanManageBroadcastPermissions: canManageBroadcastPermissions,
		CanManageBroadcastGroups:      canManageBroadcastGroups,
		ChatNames:                     chatNames,
		OpLabels:                      opLabels,
		GroupPager:                    groupPager,
		BroadcastPager:                broadcastPager,
		OperatorPager:                 operatorPager,
		PermissionPager:               permissionPager,
	}, nil
}

func parseAdminListFilters(r *http.Request) adminListFilters {
	query := r.URL.Query()
	return adminListFilters{
		GroupQuery:      strings.TrimSpace(query.Get("groups_q")),
		GroupPage:       parseAdminPage(query.Get("groups_page")),
		BroadcastQuery:  strings.TrimSpace(query.Get("broadcast_q")),
		BroadcastPage:   parseAdminPage(query.Get("broadcast_page")),
		OperatorQuery:   strings.TrimSpace(query.Get("operators_q")),
		OperatorPage:    parseAdminPage(query.Get("operators_page")),
		PermissionQuery: strings.TrimSpace(query.Get("permissions_q")),
		PermissionPage:  parseAdminPage(query.Get("permissions_page")),
	}
}

func parseAdminPage(value string) int {
	page, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

func adminPagerFor(filters adminListFilters, kind, query string, requestedPage, total int) adminPager {
	if requestedPage < 1 {
		requestedPage = 1
	}
	lastPage := (total + adminListPageSize - 1) / adminListPageSize
	if lastPage < 1 {
		lastPage = 1
	}
	if requestedPage > lastPage {
		requestedPage = lastPage
	}
	from := (requestedPage - 1) * adminListPageSize
	to := from + adminListPageSize
	if to > total {
		to = total
	}
	pager := adminPager{Query: query, Page: requestedPage, HasPrev: requestedPage > 1, HasNext: requestedPage < lastPage, Total: total}
	if total > 0 {
		pager.ItemFrom = from + 1
		pager.ItemTo = to
	}
	pager.PrevURL = adminListURL(filters, kind, requestedPage-1)
	pager.NextURL = adminListURL(filters, kind, requestedPage+1)
	return pager
}

func adminListURL(filters adminListFilters, kind string, page int) string {
	values := url.Values{}
	set := func(name, query string, currentPage int) {
		if query != "" {
			values.Set(name+"_q", query)
		}
		if currentPage > 1 {
			values.Set(name+"_page", strconv.Itoa(currentPage))
		}
	}
	pages := map[string]int{
		"groups": filters.GroupPage, "broadcast": filters.BroadcastPage,
		"operators": filters.OperatorPage, "permissions": filters.PermissionPage,
	}
	pages[kind] = page
	set("groups", filters.GroupQuery, pages["groups"])
	set("broadcast", filters.BroadcastQuery, pages["broadcast"])
	set("operators", filters.OperatorQuery, pages["operators"])
	set("permissions", filters.PermissionQuery, pages["permissions"])
	if encoded := values.Encode(); encoded != "" {
		return "/admin?" + encoded
	}
	return "/admin"
}

func pageAdminGroups(items []storage.Group, filters adminListFilters) ([]storage.Group, adminPager) {
	query := strings.ToLower(filters.GroupQuery)
	filtered := make([]storage.Group, 0, len(items))
	for _, item := range items {
		if query == "" || strings.Contains(strings.ToLower(item.Title), query) || strings.Contains(strconv.FormatInt(item.ChatID, 10), query) {
			filtered = append(filtered, item)
		}
	}
	pager := adminPagerFor(filters, "groups", filters.GroupQuery, filters.GroupPage, len(filtered))
	return filtered[(pager.Page-1)*adminListPageSize : pager.ItemTo], pager
}

func pageAdminBroadcastGroups(items []storage.BroadcastGroup, filters adminListFilters) ([]storage.BroadcastGroup, adminPager) {
	query := strings.ToLower(filters.BroadcastQuery)
	filtered := make([]storage.BroadcastGroup, 0, len(items))
	for _, item := range items {
		haystack := strings.ToLower(item.Name + " " + strings.Join(item.ChatNames, " "))
		if query == "" || strings.Contains(haystack, query) {
			filtered = append(filtered, item)
		}
	}
	pager := adminPagerFor(filters, "broadcast", filters.BroadcastQuery, filters.BroadcastPage, len(filtered))
	return filtered[(pager.Page-1)*adminListPageSize : pager.ItemTo], pager
}

func pageAdminOperators(items []storage.GlobalOperator, filters adminListFilters) ([]storage.GlobalOperator, adminPager) {
	query := strings.ToLower(filters.OperatorQuery)
	filtered := make([]storage.GlobalOperator, 0, len(items))
	for _, item := range items {
		haystack := strings.ToLower(strings.Join([]string{operatorLabel(item), item.Username, item.DisplayName, item.Remark, maskedUserID(item.UserID)}, " "))
		if query == "" || strings.Contains(haystack, query) {
			filtered = append(filtered, item)
		}
	}
	pager := adminPagerFor(filters, "operators", filters.OperatorQuery, filters.OperatorPage, len(filtered))
	return filtered[(pager.Page-1)*adminListPageSize : pager.ItemTo], pager
}

func pageAdminPermissions(items []storage.BroadcastPermission, chatNames, opLabels map[int64]string, filters adminListFilters) ([]storage.BroadcastPermission, adminPager) {
	query := strings.ToLower(filters.PermissionQuery)
	filtered := make([]storage.BroadcastPermission, 0, len(items))
	for _, item := range items {
		haystack := strings.ToLower(strings.Join([]string{permissionUserLabel(item, opLabels), permissionTarget(item, chatNames), grantorLabel(item, opLabels)}, " "))
		if query == "" || strings.Contains(haystack, query) {
			filtered = append(filtered, item)
		}
	}
	pager := adminPagerFor(filters, "permissions", filters.PermissionQuery, filters.PermissionPage, len(filtered))
	return filtered[(pager.Page-1)*adminListPageSize : pager.ItemTo], pager
}

func (s *Server) privateCleanupConfigOperator(ctx context.Context, userID int64, level, fallbackLabel string) (storage.GlobalOperator, error) {
	op := storage.GlobalOperator{UserID: userID, Level: level, Status: "active", Remark: fallbackLabel}
	if user, ok, err := s.store.GetLatestUserIdentity(ctx, userID); err != nil {
		return op, err
	} else if ok {
		op.Username = user.Username
		op.DisplayName = user.DisplayName
	}
	if settings, ok, err := s.store.GetPrivateCleanupSettings(ctx, userID); err != nil {
		return op, err
	} else if ok {
		op.PrivateCleanupEnabled = settings.Enabled
		op.PrivateCleanupTime = settings.DailyTime
		op.PrivateCleanupLastRunDate = settings.DailyLastRunDate
		op.PrivateCleanupBotDeleteAfterSeconds = settings.BotDeleteAfter
		op.PrivateCleanupIncomingEnabled = settings.IncomingEnabled
		op.PrivateCleanupIncomingAfterSeconds = settings.IncomingDeleteAfter
		op.PrivateCleanupScope = settings.Scope
	}
	return op, nil
}

func redirectMsg(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin?msg="+template.URLQueryEscaper(msg), http.StatusSeeOther)
}

func parseIDList(values []string) []int64 {
	var ids []int64
	for _, value := range values {
		parts := strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
		})
		for _, part := range parts {
			id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
			if err == nil && id != 0 {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func renderLogin(w http.ResponseWriter, tokenUnset bool, message string) {
	renderLoginWithTicket(w, tokenUnset, message, "")
}

func renderLoginWithTicket(w http.ResponseWriter, tokenUnset bool, message, ticket string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTemplate.Execute(w, map[string]any{"TokenUnset": tokenUnset, "Message": message, "Ticket": ticket})
}

func chatLabel(group storage.Group) string {
	if group.Title != "" {
		return group.Title
	}
	return strconv.FormatInt(group.ChatID, 10)
}

func chatBroadcastGroups(group storage.Group, bgroups []storage.BroadcastGroup) string {
	names := make([]string, 0, 2)
	for _, bgroup := range bgroups {
		for _, chatID := range bgroup.ChatIDs {
			if chatID == group.ChatID {
				names = append(names, bgroup.Name)
				break
			}
		}
	}
	payload, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(payload)
}

func permissionGroupUsers(groupName string, permissions []storage.BroadcastPermission) string {
	userIDs := make([]int64, 0, 2)
	for _, permission := range permissions {
		if permission.Target == "group" && permission.GroupName == groupName {
			userIDs = append(userIDs, permission.UserID)
		}
	}
	return userIDsJSON(userIDs)
}

func permissionChatUsers(group storage.Group, permissions []storage.BroadcastPermission) string {
	userIDs := make([]int64, 0, 2)
	for _, permission := range permissions {
		if permission.Target == "chat" && permission.ChatID == group.ChatID {
			userIDs = append(userIDs, permission.UserID)
		}
	}
	return userIDsJSON(userIDs)
}

func userIDsJSON(userIDs []int64) string {
	seen := make(map[int64]struct{}, len(userIDs))
	unique := make([]int64, 0, len(userIDs))
	for _, userID := range userIDs {
		if _, ok := seen[userID]; ok {
			continue
		}
		seen[userID] = struct{}{}
		unique = append(unique, userID)
	}
	sort.Slice(unique, func(i, j int) bool { return unique[i] < unique[j] })
	values := make([]string, 0, len(unique))
	for _, userID := range unique {
		values = append(values, strconv.FormatInt(userID, 10))
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(payload)
}

func permissionTarget(p storage.BroadcastPermission, names map[int64]string) string {
	if p.Target == "group" {
		return "分组：" + p.GroupName
	}
	name := names[p.ChatID]
	if name == "" {
		name = strconv.FormatInt(p.ChatID, 10)
	}
	return "单群：" + name
}

func operatorLabel(op storage.GlobalOperator) string {
	remark := strings.TrimSpace(op.Remark)
	if remark != "" {
		return remark
	}
	if username := strings.TrimSpace(op.Username); username != "" {
		return "@" + username
	}
	if displayName := strings.TrimSpace(op.DisplayName); displayName != "" {
		return displayName
	}
	return "未备注操作人 " + maskedUserID(op.UserID)
}

func operatorLevelLabel(level string) string {
	switch level {
	case "primary":
		return "一级操作人"
	case "secondary":
		return "下级操作人"
	case "host":
		return "宿主"
	case "default":
		return "默认操作人"
	default:
		return level
	}
}

func operatorSourceLabel(op storage.GlobalOperator, labels map[int64]string) string {
	if op.Level == "secondary" && op.ParentUserID > 0 {
		if label := labels[op.ParentUserID]; label != "" {
			return label
		}
		return "一级授权人 " + maskedUserID(op.ParentUserID)
	}
	if op.CreatedBy == 0 {
		return "后台管理"
	}
	if label := labels[op.CreatedBy]; label != "" {
		return label
	}
	return "已授权操作人 " + maskedUserID(op.CreatedBy)
}

func adminTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02 15:04")
}

func permissionUserLabel(p storage.BroadcastPermission, labels map[int64]string) string {
	if label := labels[p.UserID]; label != "" {
		return label
	}
	return "未备注操作人 " + maskedUserID(p.UserID)
}

func grantorLabel(p storage.BroadcastPermission, labels map[int64]string) string {
	if p.GrantedBy == 0 {
		return "后台管理"
	}
	if label := labels[p.GrantedBy]; label != "" {
		return label
	}
	return "已授权操作人 " + maskedUserID(p.GrantedBy)
}

func watchOwnerLabel(target storage.WatchTarget, labels map[int64]string) string {
	if label := labels[target.OwnerUserID]; label != "" {
		return label
	}
	return "用户 " + maskedUserID(target.OwnerUserID)
}

func maskedUserID(userID int64) string {
	if userID <= 0 {
		return ""
	}
	text := strconv.FormatInt(userID, 10)
	if len(text) <= 4 {
		return "****" + text
	}
	return "****" + text[len(text)-4:]
}

func watchLatestTime(target storage.WatchTarget) string {
	if target.LatestTimestamp <= 0 {
		return "暂无"
	}
	return time.UnixMilli(target.LatestTimestamp).Format("2006-01-02 15:04:05")
}

func parseBillPath(path string) (int64, string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if (len(parts) != 3 && len(parts) != 4) || parts[0] != "b" {
		return 0, "", "", false
	}
	chatID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", "", false
	}
	day := strings.TrimSpace(parts[2])
	day = normalizeBillDay(day)
	if day == "" {
		return 0, "", "", false
	}
	action := ""
	if len(parts) == 4 {
		action = parts[3]
		if action != "download" {
			return 0, "", "", false
		}
	}
	return chatID, day, action, true
}

func billPath(chatID int64, dayKey string) string {
	return fmt.Sprintf("/b/%d/%s", chatID, strings.ReplaceAll(dayKey, "-", ""))
}

func billDownloadPath(chatID int64, dayKey, field, query string) string {
	return billPath(chatID, dayKey) + "/download" + billFilterSuffix(field, query)
}

func billCursorPath(chatID int64, dayKey, field, query, cursor string, id int64) string {
	values := url.Values{}
	if normalized := normalizedBillField(field); normalized != "all" {
		values.Set("field", normalized)
	}
	if strings.TrimSpace(query) != "" {
		values.Set("q", strings.TrimSpace(query))
	}
	if id > 0 && (cursor == "before" || cursor == "after") {
		values.Set(cursor, strconv.FormatInt(id, 10))
	}
	encoded := values.Encode()
	if encoded == "" {
		return billPath(chatID, dayKey)
	}
	return billPath(chatID, dayKey) + "?" + encoded
}

func positiveInt64(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func billFilterSuffix(field, query string) string {
	query = strings.TrimSpace(query)
	field = normalizedBillField(field)
	if query == "" {
		return ""
	}
	values := url.Values{}
	values.Set("q", query)
	if field != "all" {
		values.Set("field", field)
	}
	return "?" + values.Encode()
}

func addDay(dayKey string, delta int) string {
	day, err := time.Parse("2006-01-02", dayKey)
	if err != nil {
		return dayKey
	}
	return day.AddDate(0, 0, delta).Format("2006-01-02")
}

func normalizeBillDay(day string) string {
	day = strings.TrimSpace(day)
	if len(day) >= 10 && day[4:5] == "-" && day[7:8] == "-" {
		return day[:10]
	}
	if len(day) == 8 {
		return day[:4] + "-" + day[4:6] + "-" + day[6:]
	}
	return ""
}

func datePart(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 10 {
		return value[:10]
	}
	return ""
}

func shortDayLabel(dayKey string) string {
	day, err := time.Parse("2006-01-02", dayKey)
	if err != nil {
		return dayKey
	}
	return day.Format("01-02")
}

func buildBillHistoryLinks(chatID int64, days []string, currentDay, field, query string, limit int) []billHistoryLink {
	if limit <= 0 || limit > len(days) {
		limit = len(days)
	}
	links := make([]billHistoryLink, 0, limit)
	suffix := billFilterSuffix(field, query)
	for _, day := range days[:limit] {
		links = append(links, billHistoryLink{
			DayKey: day,
			Label:  shortDayLabel(day),
			URL:    billPath(chatID, day) + suffix,
			Active: day == currentDay,
		})
	}
	return links
}

type billRecordWalker func(kind string, visit func([]storage.Record) error) error

func writeBillXLSX(group storage.Group, dayKey string, walk billRecordWalker, output io.Writer) error {
	accumulator := newBillAccumulator(group, false)
	depositAccumulator := newBillAccumulator(group, false)
	payoutAccumulator := newBillAccumulator(group, false)
	if err := walk("", func(records []storage.Record) error {
		for _, record := range records {
			accumulator.Add(record)
			if record.Kind == "deposit" {
				depositAccumulator.Add(record)
			} else if record.Kind == "payout" {
				payoutAccumulator.Add(record)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	summary := accumulator.Summary()
	depositSummary := depositAccumulator.Summary()
	payoutSummary := payoutAccumulator.Summary()
	file := excelize.NewFile()
	defer func() { _ = file.Close() }()
	sheet := "账单"
	file.SetSheetName("Sheet1", sheet)
	titleStyle, _ := file.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "0E1B2F"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"EFDCA9"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
	})
	headerStyle, _ := file.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "0E1B2F"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"F4F7FB"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
		Border: []excelize.Border{
			{Type: "left", Color: "DCE5EF", Style: 1},
			{Type: "right", Color: "DCE5EF", Style: 1},
			{Type: "top", Color: "DCE5EF", Style: 1},
			{Type: "bottom", Color: "DCE5EF", Style: 1},
		},
	})
	cellStyle, _ := file.NewStyle(&excelize.Style{
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "E5EDF5", Style: 1},
			{Type: "right", Color: "E5EDF5", Style: 1},
			{Type: "top", Color: "E5EDF5", Style: 1},
			{Type: "bottom", Color: "E5EDF5", Style: 1},
		},
	})
	stream, err := file.NewStreamWriter(sheet)
	if err != nil {
		return err
	}
	for _, width := range []struct {
		min, max int
		value    float64
	}{{1, 1, 10}, {2, 2, 16}, {3, 3, 18}, {4, 4, 28}, {5, 5, 16}, {6, 6, 24}, {7, 8, 28}} {
		if err := stream.SetColWidth(width.min, width.max, width.value); err != nil {
			return err
		}
	}
	row := 1
	var writeErr error
	addTitle := func(text string) {
		if writeErr != nil {
			return
		}
		start, _ := excelize.CoordinatesToCellName(1, row)
		end, _ := excelize.CoordinatesToCellName(8, row)
		writeErr = stream.SetRow(start, []interface{}{excelize.Cell{StyleID: titleStyle, Value: text}})
		if writeErr == nil {
			writeErr = stream.MergeCell(start, end)
		}
		row++
	}
	addRow := func(values ...any) {
		if writeErr != nil {
			return
		}
		cells := make([]interface{}, len(values))
		for i, value := range values {
			cells[i] = excelize.Cell{StyleID: cellStyle, Value: value}
		}
		start, _ := excelize.CoordinatesToCellName(1, row)
		writeErr = stream.SetRow(start, cells)
		row++
	}
	addHeader := func(values ...any) {
		if writeErr != nil {
			return
		}
		cells := make([]interface{}, len(values))
		for i, value := range values {
			cells[i] = excelize.Cell{StyleID: headerStyle, Value: value}
		}
		start, _ := excelize.CoordinatesToCellName(1, row)
		writeErr = stream.SetRow(start, cells)
		row++
	}
	addEmpty := func() {
		row++
	}

	addTitle(fmt.Sprintf("%s  %s  【%s】", dayKey, weekdayLabel(dayKey), group.Title))
	addEmpty()
	addTitle(fmt.Sprintf("入款：%d笔", summary.DepositCount))
	addHeader("序号", "时间", "金额", "应下发", "应下发(U)", "转账人", "回复人", "操作人")
	depositIndex := 0
	if err := walk("deposit", func(records []storage.Record) error {
		for _, record := range records {
			depositIndex++
			addRow(depositIndex, billExcelTime(record.CreatedAt), billNumber(record.Amount, 2), billAmount(record), billNumber(record.ResultUSDT, 2), record.Remark, recordSubjectName(record), recordActorName(record))
		}
		return writeErr
	}); err != nil {
		return err
	}
	addEmpty()
	addPeopleXLSXSection(addTitle, addHeader, addRow, "入款回复人小计", depositSummary.SubjectStats, true)
	addEmpty()
	addPeopleXLSXSection(addTitle, addHeader, addRow, "入款操作人小计", depositSummary.ActorStats, true)
	addEmpty()
	addTitle("入款按汇率小计")
	addHeader("汇率", "入款", "换算U")
	for _, item := range summary.RateStats {
		addRow(item.Rate, item.AmountCNY, item.AmountUSDT+" U")
	}
	addEmpty()
	addTitle(fmt.Sprintf("下发：%d笔", summary.PayoutCount))
	addHeader("序号", "时间", "金额", "回复人", "操作人")
	payoutIndex := 0
	if err := walk("payout", func(records []storage.Record) error {
		for _, record := range records {
			payoutIndex++
			addRow(payoutIndex, billExcelTime(record.CreatedAt), billAmount(record), recordSubjectName(record), recordActorName(record))
		}
		return writeErr
	}); err != nil {
		return err
	}
	addEmpty()
	addPeopleXLSXSection(addTitle, addHeader, addRow, "下发回复人小计", payoutSummary.SubjectStats, false)
	addEmpty()
	addTitle("总计")
	addRow("费率：", summary.FeeRate+"%")
	addRow("汇率：", summary.ExchangeRate)
	addRow("入款总数：", summary.TotalDepositCNY+"  |  "+summary.TotalDepositGrossUSDT+" U")
	addRow("应下发：", summary.TotalDepositNetCNY+"  |  "+summary.TotalDepositNetUSDT+" U")
	addRow("已下发：", summary.TotalPayoutCNY+"  |  "+summary.TotalPayoutUSDT+" U")
	addRow("未下发：", summary.BalanceCNY+"  |  "+summary.BalanceUSDT+" U")
	if writeErr != nil {
		return writeErr
	}
	if err := stream.Flush(); err != nil {
		return err
	}
	return file.Write(output)
}

func addPeopleXLSXSection(
	addTitle func(string),
	addHeader func(...any),
	addRow func(...any),
	title string,
	stats []billPeopleStat,
	inOnly bool,
) {
	addTitle(title)
	addHeader("用户名", "笔数", "入款", "已下发", "未下发")
	for _, item := range stats {
		amount := item.OutCNY + "  |  " + item.OutUSDT + " U"
		if inOnly {
			amount = item.InCNY + "  |  " + item.InUSDT + " U"
		}
		addRow(item.Name, fmt.Sprintf("%d 笔", item.Count), amount, item.OutCNY+"  |  "+item.OutUSDT+" U", item.BalanceCNY+"  |  "+item.BalanceUSDT+" U")
	}
}

func weekdayLabel(dayKey string) string {
	day, err := time.Parse("2006-01-02", dayKey)
	if err != nil {
		return ""
	}
	labels := []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}
	return labels[int(day.Weekday())]
}

func billExcelTime(value time.Time) string {
	return value.In(beijingLocation()).Format("15:04:05")
}

func billDisplayTime(value time.Time) string {
	return value.In(beijingLocation()).Format("01-02 15:04:05")
}

func beijingLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}

func safeFileName(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	replacer := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return replacer.Replace(value)
}

func billAmount(record storage.Record) string {
	amount := billNumber(record.Amount, 2)
	if strings.EqualFold(record.Currency, "USDT") {
		return amount + "U"
	}
	rate := billNumber(record.Rate, 8)
	if rate == "" || rate == "1" {
		return amount
	}
	result := billNumber(record.ResultUSDT, 2)
	if result == "" {
		return amount + "/" + rate
	}
	if record.Kind == "payout" {
		return result + "U/" + amount
	}
	if factor := billFeeFactorText(record.FeeRate); factor != "" {
		return amount + "/" + rate + "*" + factor + "=" + result + "U"
	}
	return amount + "/" + rate + "=" + result + "U"
}

func billAmountHTML(record storage.Record) template.HTML {
	display := billAmount(record)
	link := originalRecordMessageURL(record)
	if link == "" {
		return template.HTML(template.HTMLEscapeString(display))
	}
	amount := billNumber(record.Amount, 2)
	if strings.EqualFold(record.Currency, "USDT") {
		amount += "U"
	}
	remainder := strings.TrimPrefix(display, amount)
	return template.HTML(`<a class="message-link copyable" href="` + template.HTMLEscapeString(link) + `">` +
		template.HTMLEscapeString(amount) + `</a>` + template.HTMLEscapeString(remainder))
}

func recordSubjectHTML(record storage.Record) template.HTML {
	name := template.HTMLEscapeString(recordSubjectName(record))
	link := originalRecordMessageURL(record)
	if link == "" {
		return template.HTML(name)
	}
	return template.HTML(`<a class="message-link" href="` + template.HTMLEscapeString(link) + `">` + name + `</a>`)
}

func originalRecordMessageURL(record storage.Record) string {
	if record.SourceMessageID <= 0 {
		return ""
	}
	rawChatID := strconv.FormatInt(record.ChatID, 10)
	if !strings.HasPrefix(rawChatID, "-100") || len(rawChatID) <= 4 {
		return ""
	}
	return "https://t.me/c/" + strings.TrimPrefix(rawChatID, "-100") + "/" + strconv.FormatInt(record.SourceMessageID, 10)
}

func billKind(kind string) string {
	if kind == "payout" {
		return "下发"
	}
	return "入款"
}

func summarizeBill(group storage.Group, records []storage.Record) billSummary {
	accumulator := newBillAccumulator(group, true)
	for _, record := range records {
		accumulator.Add(record)
	}
	return accumulator.Summary()
}

type billAccumulator struct {
	summary               billSummary
	keepRecords           bool
	totalDepositCNY       *big.Rat
	totalDepositGrossUSDT *big.Rat
	totalDepositNetCNY    *big.Rat
	totalDepositNetUSDT   *big.Rat
	totalPayoutCNY        *big.Rat
	totalPayoutUSDT       *big.Rat
	commissionCNY         *big.Rat
	subjectStats          map[string]*billPeopleStatAccumulator
	actorStats            map[string]*billPeopleStatAccumulator
	remarkStats           map[string]*billPeopleStatAccumulator
	rateStats             map[string]*billRateAccumulator
}

func newBillAccumulator(group storage.Group, keepRecords bool) *billAccumulator {
	return &billAccumulator{
		summary:               billSummary{ExchangeRate: billExchangeRateDisplay(group), FeeRate: group.FeeRate},
		keepRecords:           keepRecords,
		totalDepositCNY:       newBillRat(),
		totalDepositGrossUSDT: newBillRat(),
		totalDepositNetCNY:    newBillRat(),
		totalDepositNetUSDT:   newBillRat(),
		totalPayoutCNY:        newBillRat(),
		totalPayoutUSDT:       newBillRat(),
		commissionCNY:         newBillRat(),
		subjectStats:          map[string]*billPeopleStatAccumulator{},
		actorStats:            map[string]*billPeopleStatAccumulator{},
		remarkStats:           map[string]*billPeopleStatAccumulator{},
		rateStats:             map[string]*billRateAccumulator{},
	}
}

func (a *billAccumulator) Add(record storage.Record) {
	switch record.Kind {
	case "deposit":
		a.summary.DepositCount++
		if a.keepRecords {
			a.summary.Deposits = append(a.summary.Deposits, record)
		}
		amountCNY := recordCNYAmount(record)
		grossUSDT := recordGrossUSDT(record)
		netUSDT := recordResultUSDT(record)
		rate := recordRateRat(record)
		netCNY := mulBillRat(netUSDT, rate)
		a.totalDepositCNY.Add(a.totalDepositCNY, amountCNY)
		a.totalDepositGrossUSDT.Add(a.totalDepositGrossUSDT, grossUSDT)
		a.totalDepositNetCNY.Add(a.totalDepositNetCNY, netCNY)
		a.totalDepositNetUSDT.Add(a.totalDepositNetUSDT, netUSDT)
		commission := new(big.Rat).Sub(grossUSDT, netUSDT)
		a.commissionCNY.Add(a.commissionCNY, mulBillRat(commission, rate))
		addPeopleDeposit(a.subjectStats, recordSubjectName(record), amountCNY, grossUSDT, netCNY, netUSDT)
		addPeopleDeposit(a.actorStats, recordActorName(record), amountCNY, grossUSDT, netCNY, netUSDT)
		addPeopleDeposit(a.remarkStats, record.Remark, amountCNY, grossUSDT, netCNY, netUSDT)
		rateKey := formatBillRat(rate, 4)
		item := a.rateStats[rateKey]
		if item == nil {
			item = &billRateAccumulator{rate: rateKey, amountCNY: newBillRat(), amountUSDT: newBillRat()}
			a.rateStats[rateKey] = item
		}
		item.amountCNY.Add(item.amountCNY, amountCNY)
		item.amountUSDT.Add(item.amountUSDT, grossUSDT)
	case "payout":
		a.summary.PayoutCount++
		if a.keepRecords {
			a.summary.Payouts = append(a.summary.Payouts, record)
		}
		amountCNY := recordCNYAmount(record)
		amountUSDT := recordResultUSDT(record)
		a.totalPayoutCNY.Add(a.totalPayoutCNY, amountCNY)
		a.totalPayoutUSDT.Add(a.totalPayoutUSDT, amountUSDT)
		addPeoplePayout(a.subjectStats, recordSubjectName(record), amountCNY, amountUSDT)
		addPeoplePayout(a.actorStats, recordActorName(record), amountCNY, amountUSDT)
		addPeoplePayout(a.remarkStats, record.Remark, amountCNY, amountUSDT)
	}
}

func (a *billAccumulator) Summary() billSummary {
	summary := a.summary
	summary.TotalDepositCNY = formatBillRat(a.totalDepositCNY, 2)
	summary.TotalDepositGrossUSDT = formatBillRat(a.totalDepositGrossUSDT, 2)
	summary.TotalDepositNetCNY = formatBillRat(a.totalDepositNetCNY, 2)
	summary.TotalDepositNetUSDT = formatBillRat(a.totalDepositNetUSDT, 2)
	summary.TotalPayoutCNY = formatBillRat(a.totalPayoutCNY, 2)
	summary.TotalPayoutUSDT = formatBillRat(a.totalPayoutUSDT, 2)
	summary.BalanceCNY = formatBillRat(new(big.Rat).Sub(a.totalDepositNetCNY, a.totalPayoutCNY), 2)
	summary.BalanceUSDT = formatBillRat(new(big.Rat).Sub(a.totalDepositNetUSDT, a.totalPayoutUSDT), 2)
	summary.CommissionCNY = formatBillRat(a.commissionCNY, 2)
	summary.SubjectStats = buildPeopleStats(a.subjectStats)
	summary.ActorStats = buildPeopleStats(a.actorStats)
	summary.RemarkStats = buildPeopleStats(a.remarkStats)
	summary.RateStats = buildRateStats(a.rateStats)
	if summary.FeeRate == "" {
		summary.FeeRate = "0"
	}
	summary.FeeRate = billNumber(summary.FeeRate, 2)
	return summary
}

type billPeopleStatAccumulator struct {
	name    string
	count   int
	inCNY   *big.Rat
	inUSDT  *big.Rat
	netCNY  *big.Rat
	netUSDT *big.Rat
	outCNY  *big.Rat
	outUSDT *big.Rat
}

type billRateAccumulator struct {
	rate       string
	amountCNY  *big.Rat
	amountUSDT *big.Rat
}

func newBillRat() *big.Rat {
	return big.NewRat(0, 1)
}

func mulBillRat(a, b *big.Rat) *big.Rat {
	if a == nil || b == nil {
		return newBillRat()
	}
	return new(big.Rat).Mul(a, b)
}

func recordRat(raw string) *big.Rat {
	value := parseBillRat(raw)
	if value == nil {
		return newBillRat()
	}
	return value
}

func recordRateRat(record storage.Record) *big.Rat {
	rate := parseBillRat(record.Rate)
	if rate == nil || rate.Sign() == 0 {
		return big.NewRat(1, 1)
	}
	return rate
}

func recordGrossUSDT(record storage.Record) *big.Rat {
	amount := recordRat(record.Amount)
	if strings.EqualFold(record.Currency, "USDT") {
		return amount
	}
	return new(big.Rat).Quo(amount, recordRateRat(record))
}

func recordResultUSDT(record storage.Record) *big.Rat {
	result := parseBillRat(record.ResultUSDT)
	if result != nil {
		return result
	}
	return recordGrossUSDT(record)
}

func recordCNYAmount(record storage.Record) *big.Rat {
	amount := recordRat(record.Amount)
	if strings.EqualFold(record.Currency, "CNY") {
		return amount
	}
	return mulBillRat(amount, recordRateRat(record))
}

func recordSubjectName(record storage.Record) string {
	if strings.TrimSpace(record.SubjectName) != "" {
		return strings.TrimSpace(record.SubjectName)
	}
	return recordActorName(record)
}

func recordActorName(record storage.Record) string {
	if strings.TrimSpace(record.ActorName) != "" {
		return strings.TrimSpace(record.ActorName)
	}
	return "未命名"
}

func peopleAccumulator(items map[string]*billPeopleStatAccumulator, name string) *billPeopleStatAccumulator {
	name = strings.TrimSpace(name)
	if name == "" {
		name = ""
	}
	item := items[name]
	if item == nil {
		item = &billPeopleStatAccumulator{
			name:    name,
			inCNY:   newBillRat(),
			inUSDT:  newBillRat(),
			netCNY:  newBillRat(),
			netUSDT: newBillRat(),
			outCNY:  newBillRat(),
			outUSDT: newBillRat(),
		}
		items[name] = item
	}
	return item
}

func addPeopleDeposit(items map[string]*billPeopleStatAccumulator, name string, inCNY, inUSDT, netCNY, netUSDT *big.Rat) {
	item := peopleAccumulator(items, name)
	item.count++
	item.inCNY.Add(item.inCNY, inCNY)
	item.inUSDT.Add(item.inUSDT, inUSDT)
	item.netCNY.Add(item.netCNY, netCNY)
	item.netUSDT.Add(item.netUSDT, netUSDT)
}

func addPeoplePayout(items map[string]*billPeopleStatAccumulator, name string, outCNY, outUSDT *big.Rat) {
	item := peopleAccumulator(items, name)
	item.count++
	item.outCNY.Add(item.outCNY, outCNY)
	item.outUSDT.Add(item.outUSDT, outUSDT)
}

func buildPeopleStats(items map[string]*billPeopleStatAccumulator) []billPeopleStat {
	stats := make([]billPeopleStat, 0, len(items))
	for _, item := range items {
		balanceCNY := new(big.Rat).Sub(item.netCNY, item.outCNY)
		balanceUSDT := new(big.Rat).Sub(item.netUSDT, item.outUSDT)
		stats = append(stats, billPeopleStat{
			Name:        item.name,
			Count:       item.count,
			InCNY:       formatBillRat(item.inCNY, 2),
			InUSDT:      formatBillRat(item.inUSDT, 2),
			OutCNY:      formatBillRat(item.outCNY, 2),
			OutUSDT:     formatBillRat(item.outUSDT, 2),
			BalanceCNY:  formatBillRat(balanceCNY, 2),
			BalanceUSDT: formatBillRat(balanceUSDT, 2),
		})
	}
	sort.Slice(stats, func(i, j int) bool {
		return stats[i].Name < stats[j].Name
	})
	return stats
}

func buildRateStats(items map[string]*billRateAccumulator) []billRateStat {
	stats := make([]billRateStat, 0, len(items))
	for _, item := range items {
		stats = append(stats, billRateStat{
			Rate:       item.rate,
			AmountCNY:  formatBillRat(item.amountCNY, 2),
			AmountUSDT: formatBillRat(item.amountUSDT, 2),
		})
	}
	sort.Slice(stats, func(i, j int) bool {
		left := parseBillRat(stats[i].Rate)
		right := parseBillRat(stats[j].Rate)
		if left != nil && right != nil {
			return left.Cmp(right) < 0
		}
		return stats[i].Rate < stats[j].Rate
	})
	return stats
}

func billFeeFactorText(raw string) string {
	fee := parseBillRat(raw)
	if fee == nil || fee.Sign() == 0 {
		return ""
	}
	factor := big.NewRat(100, 1)
	factor.Sub(factor, fee)
	factor.Quo(factor, big.NewRat(100, 1))
	return formatBillRat(factor, 4)
}

func billExchangeRateDisplay(group storage.Group) string {
	if group.ExchangeRateSource != "" && group.ExchangeRateRank > 0 {
		source := strings.TrimSpace(group.ExchangeRateSource)
		if source == "" {
			source = "支付宝"
		}
		label := source + strconv.Itoa(group.ExchangeRateRank) + "档"
		offset := parseBillRat(group.ExchangeRateOffset)
		if offset == nil || offset.Sign() == 0 {
			return label
		}
		if offset.Sign() > 0 {
			return label + " 上浮" + formatBillRat(offset, 8)
		}
		abs := new(big.Rat).Neg(offset)
		return label + " 下浮" + formatBillRat(abs, 8)
	}
	rate := billNumber(group.DepositExchangeRate, 8)
	if rate == "" {
		return "1"
	}
	return rate
}

func parseBillRat(raw string) *big.Rat {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	value, ok := new(big.Rat).SetString(raw)
	if !ok {
		return nil
	}
	return value
}

func formatBillRat(value *big.Rat, precision int) string {
	if value == nil {
		return "0"
	}
	text := value.FloatString(precision)
	text = strings.TrimRight(strings.TrimRight(text, "0"), ".")
	if text == "" || text == "-0" {
		return "0"
	}
	return text
}

func billNumber(raw string, precision int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	value := parseBillRat(raw)
	if value == nil {
		return raw
	}
	return formatBillRat(value, precision)
}

func billQueryText(r *http.Request) string {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query != "" {
		return query
	}
	return strings.TrimSpace(r.URL.Query().Get("firstname"))
}

func billQueryField(r *http.Request) string {
	field := strings.TrimSpace(r.URL.Query().Get("field"))
	if field != "" {
		return normalizedBillField(field)
	}
	return legacyBillType(r.URL.Query().Get("type"))
}

func legacyBillType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "bjr", "subject":
		return "subject"
	case "czr", "actor":
		return "actor"
	case "bz", "remark":
		return "remark"
	case "amount":
		return "amount"
	default:
		return "all"
	}
}

func normalizedBillField(field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "subject", "actor", "remark", "amount", "bjr", "czr", "bz":
		return legacyBillType(field)
	default:
		return "all"
	}
}

func filterBillRecords(records []storage.Record, query, field string) []storage.Record {
	query = strings.TrimSpace(query)
	if query == "" {
		return records
	}
	field = normalizedBillField(field)
	filtered := make([]storage.Record, 0, len(records))
	for _, record := range records {
		if billRecordMatches(record, field, query) {
			filtered = append(filtered, record)
		}
	}
	return filtered
}

func billRecordMatches(record storage.Record, field, query string) bool {
	switch field {
	case "subject":
		return containsFold(recordSubjectName(record), query)
	case "actor":
		return containsFold(record.ActorName, query)
	case "remark":
		return containsFold(record.Remark, query)
	case "amount":
		return containsFold(record.Amount, query) ||
			containsFold(record.ResultUSDT, query) ||
			containsFold(record.Rate, query) ||
			containsFold(billAmount(record), query)
	default:
		values := []string{
			billKind(record.Kind),
			record.CreatedAt.Format("2006-01-02 15:04:05"),
			record.Amount,
			record.Rate,
			record.FeeRate,
			record.ResultUSDT,
			record.Currency,
			record.ActorName,
			record.Remark,
			billAmount(record),
		}
		for _, value := range values {
			if containsFold(value, query) {
				return true
			}
		}
		return false
	}
}

func containsFold(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}

func normalizeAdminMinAmount(raw string) string {
	value := parseBillRat(strings.TrimSpace(raw))
	if value == nil || value.Sign() < 0 {
		return "0"
	}
	return formatBillRat(value, 2)
}

func normalizeCleanupTime(raw string) (string, bool) {
	minutes, ok := storage.CleanupTimeMinutes(raw)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60), true
}

func normalizeCleanupTimeParts(hourRaw, minuteRaw string) (string, bool) {
	hourRaw = strings.TrimSpace(hourRaw)
	minuteRaw = strings.TrimSpace(minuteRaw)
	if hourRaw == "" && minuteRaw == "" {
		return "", true
	}
	if hourRaw == "" || minuteRaw == "" || !decimalDigitsOnly(hourRaw) || !decimalDigitsOnly(minuteRaw) {
		return "", false
	}
	hour, hourErr := strconv.Atoi(hourRaw)
	minute, minuteErr := strconv.Atoi(minuteRaw)
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return "", false
	}
	return fmt.Sprintf("%02d:%02d", hour, minute), true
}

func decimalDigitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func cleanupTimeHour(value string) string {
	return cleanupTimePart(value, 0)
}

func cleanupTimeMinute(value string) string {
	return cleanupTimePart(value, 1)
}

func cleanupTimePart(value string, index int) string {
	normalized, ok := normalizeCleanupTime(value)
	if !ok {
		return ""
	}
	parts := strings.Split(normalized, ":")
	if len(parts) != 2 || index < 0 || index >= len(parts) {
		return ""
	}
	number, err := strconv.Atoi(parts[index])
	if err != nil {
		return ""
	}
	return strconv.Itoa(number)
}

func normalizeCleanupDelay(preset string, custom string, unit string) (int, bool) {
	preset = strings.TrimSpace(preset)
	if preset == "" {
		return 0, true
	}
	if preset != "custom" {
		seconds, err := strconv.Atoi(preset)
		if err != nil || seconds < 0 || seconds > 86400 {
			return 0, false
		}
		return seconds, true
	}
	value, err := strconv.Atoi(strings.TrimSpace(custom))
	if err != nil || value < 1 {
		return 0, false
	}
	switch strings.TrimSpace(unit) {
	case "seconds", "秒":
		if value > 86400 {
			return 0, false
		}
		return value, true
	default:
		if value > 1440 {
			return 0, false
		}
		return value * 60, true
	}
}

func cleanupDelayPreset(seconds int) string {
	switch seconds {
	case 0, 30, 60, 300, 600, 1800, 3600:
		return strconv.Itoa(seconds)
	default:
		return "custom"
	}
}

func cleanupDelayCustomValue(seconds int) string {
	if cleanupDelayPreset(seconds) != "custom" {
		return ""
	}
	if seconds%60 == 0 {
		return strconv.Itoa(seconds / 60)
	}
	return strconv.Itoa(seconds)
}

func cleanupDelayCustomUnit(seconds int) string {
	if cleanupDelayPreset(seconds) != "custom" {
		return "minutes"
	}
	if seconds%60 == 0 {
		return "minutes"
	}
	return "seconds"
}

func cleanupSummary(op storage.GlobalOperator) string {
	if !op.PrivateCleanupEnabled {
		return "关闭"
	}
	var parts []string
	if op.PrivateCleanupBotDeleteAfterSeconds > 0 {
		parts = append(parts, "bot提示 "+cleanupDelayLabel(op.PrivateCleanupBotDeleteAfterSeconds))
	}
	if op.PrivateCleanupIncomingEnabled {
		if op.PrivateCleanupIncomingAfterSeconds > 0 {
			parts = append(parts, "用户临时消息 "+cleanupDelayLabel(op.PrivateCleanupIncomingAfterSeconds))
		} else if op.PrivateCleanupTime != "" {
			parts = append(parts, "用户临时消息 每日兜底")
		}
	}
	if op.PrivateCleanupTime != "" {
		parts = append(parts, "每日 "+op.PrivateCleanupTime)
	}
	if len(parts) == 0 {
		return "已开启，未选择规则"
	}
	return strings.Join(parts, "；")
}

func cleanupScopeEnabled(scope, category string) bool {
	return storage.PrivateCleanupScopeIncludes(scope, category)
}

func cleanupDelayLabel(seconds int) string {
	switch seconds {
	case 0:
		return "不清理"
	case 30:
		return "30秒后"
	case 60:
		return "1分钟后"
	case 300:
		return "5分钟后"
	case 600:
		return "10分钟后"
	case 1800:
		return "30分钟后"
	case 3600:
		return "1小时后"
	default:
		if seconds%60 == 0 {
			return fmt.Sprintf("%d分钟后", seconds/60)
		}
		return fmt.Sprintf("%d秒后", seconds)
	}
}

func outboxErrorHint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "":
		return "无错误"
	case strings.Contains(value, "retry_after") || strings.Contains(value, " 429 "):
		return "Telegram 限流 429，按 retry_after 延后重试"
	case strings.Contains(value, " 500 ") || strings.Contains(value, " 502 ") || strings.Contains(value, " 503 ") || strings.Contains(value, " 504 "):
		return "Telegram 5xx/网关错误，有限重试后仍失败会保留 failed"
	case strings.Contains(value, "timeout") || strings.Contains(value, "deadline exceeded") || strings.Contains(value, "connection") || strings.Contains(value, "network"):
		return "网络超时或连接抖动，有限重试后仍失败会保留 failed"
	case strings.Contains(value, "queue is full"):
		return "本地通知队列已满，稍后重试"
	default:
		return "普通发送失败，查看最近错误原文"
	}
}

func privateCleanupLocation() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*3600)
	}
	return loc
}

func canMutateOperatorRow(op storage.GlobalOperator, canManageGlobal bool, adminUserID int64) bool {
	if canManageGlobal {
		return op.Level == "primary" || op.Level == "secondary"
	}
	return op.Level == "secondary" && op.ParentUserID == adminUserID
}

func canManageBroadcastGroupRow(group storage.BroadcastGroup, canManageGlobal bool, adminUserID int64) bool {
	return canManageGlobal || (adminUserID > 0 && group.OwnerUserID == adminUserID)
}

var adminTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{
	"chatLabel":               chatLabel,
	"chatBroadcastGroups":     chatBroadcastGroups,
	"permissionGroupUsers":    permissionGroupUsers,
	"permissionChatUsers":     permissionChatUsers,
	"operatorLabel":           operatorLabel,
	"operatorLevelLabel":      operatorLevelLabel,
	"operatorSourceLabel":     operatorSourceLabel,
	"adminTime":               adminTime,
	"permissionTarget":        permissionTarget,
	"permissionUserLabel":     permissionUserLabel,
	"grantorLabel":            grantorLabel,
	"watchOwnerLabel":         watchOwnerLabel,
	"watchLatestTime":         watchLatestTime,
	"cleanupSummary":          cleanupSummary,
	"cleanupDelayPreset":      cleanupDelayPreset,
	"cleanupDelayCustomValue": cleanupDelayCustomValue,
	"cleanupDelayCustomUnit":  cleanupDelayCustomUnit,
	"cleanupScopeEnabled":     cleanupScopeEnabled,
	"cleanupTimeHour":         cleanupTimeHour,
	"cleanupTimeMinute":       cleanupTimeMinute,
	"canMutateOperator":       canMutateOperatorRow,
	"canManageBroadcastGroup": canManageBroadcastGroupRow,
}).Parse(adminHTML))

var loginTemplate = template.Must(template.New("login").Parse(loginHTML))

var billTemplate = template.Must(template.New("bill").Funcs(template.FuncMap{
	"billAmount":        billAmount,
	"billAmountHTML":    billAmountHTML,
	"billKind":          billKind,
	"billTime":          billDisplayTime,
	"recordSubject":     recordSubjectName,
	"recordSubjectHTML": recordSubjectHTML,
	"recordActor":       recordActorName,
}).Parse(billHTML))

const loginHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>后台登录</title>
<style>
body{margin:0;background:#edf3f8;color:#102033;font-family:Arial,"Microsoft YaHei",sans-serif}
.box{width:360px;max-width:calc(100% - 32px);margin:12vh auto;background:#fff;border:1px solid #cbd8e6;border-top:4px solid #d7b35d;border-radius:8px;padding:28px}
h1{margin:0 0 20px;font-size:24px}
input,button{width:100%;height:42px;box-sizing:border-box;border-radius:6px;border:1px solid #b9cadc;font-size:15px}
input{padding:0 12px;margin-bottom:12px}
button{background:#12213a;color:#fff;font-weight:700;cursor:pointer}
.warn{background:#fff7dd;border:1px solid #e1bd5f;border-radius:6px;padding:10px;margin-bottom:12px}
.err{color:#b42318;margin-bottom:12px}
</style>
</head>
<body><main class="box">
<h1>后台管理登录</h1>
{{if .TokenUnset}}<div class="warn">当前没有配置 ADMIN_WEB_TOKEN，无法创建后台会话；公网部署请务必设置。</div>{{end}}
{{if .Message}}<div class="err">{{.Message}}</div>{{end}}
{{if .Ticket}}<form method="post" action="/admin/login">
<input type="hidden" name="ticket" value="{{.Ticket}}">
<button type="submit">使用快捷登录进入后台</button>
</form>
<div class="warn">如果快捷登录失败，也可以输入后台密码登录。</div>{{end}}
<form method="post" action="/admin/login">
<input type="password" name="password" placeholder="输入后台密码">
<button type="submit">进入后台</button>
</form>
</main></body></html>`

const adminHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Telegram 记账机器人后台</title>
<style>
:root{--bg:#eaf1f7;--panel:#fff;--line:#c8d6e6;--ink:#0e1b2f;--muted:#5b6f88;--navy:#14223a;--gold:#d8b45d;--blue:#2d6cdf;--soft:#f5f8fc}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font-family:Arial,"Microsoft YaHei",sans-serif;font-size:14px}
.wrap{max-width:1240px;margin:0 auto;padding:22px}
.top{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:18px 20px;display:flex;justify-content:space-between;gap:16px;align-items:center}
.brand{color:#b97914;font-weight:700;margin-bottom:5px}.title{font-size:28px;font-weight:800}.sub{color:var(--muted)}
.btn{height:38px;border:0;border-radius:6px;background:var(--navy);color:#fff;font-weight:700;padding:0 16px;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;justify-content:center}
.btn.secondary{background:#fff;color:var(--navy);border:1px solid var(--line)}
.msg{margin-top:14px;background:#eef8ff;border:1px solid #b8d8ff;color:#16437b;border-radius:6px;padding:10px 12px}
.warn{margin-top:14px;background:#fff7dd;border:1px solid #e1bd5f;border-radius:6px;padding:10px 12px}
.tabs{margin-top:16px;display:flex;gap:8px;flex-wrap:wrap}.tab-btn{height:38px;border:1px solid var(--line);border-radius:6px;background:#fff;color:var(--navy);font-weight:800;padding:0 14px;cursor:pointer}.tab-btn.active{background:var(--navy);color:#fff;border-color:var(--navy)}
.grid{margin-top:16px;display:grid;grid-template-columns:minmax(0,1fr);gap:16px;align-items:start}
.card{background:var(--panel);border:1px solid var(--line);border-top:4px solid var(--gold);border-radius:8px;padding:18px;min-width:0}
.card.wide{grid-column:auto}
.tab-card{display:none}.tab-card.active{display:block}
h2{font-size:21px;margin:0 0 12px}.hint{color:var(--muted);margin:0 0 12px;line-height:1.55}
.row{display:grid;grid-template-columns:1fr 1fr auto;gap:8px;margin-bottom:8px}.row.two{grid-template-columns:1fr auto}.row.one{grid-template-columns:1fr}
input,select,textarea{border:1px solid #b8c8dc;border-radius:6px;background:#fff;color:var(--ink);min-height:38px;padding:8px 10px;font-size:14px;min-width:0}
select[multiple]{min-height:150px}.full{width:100%}
table{width:100%;border-collapse:collapse;margin-top:10px}th,td{border:1px solid #dce5ef;padding:10px;text-align:center;vertical-align:middle}th{background:#f4f7fb;font-weight:800}
.table-tools{display:flex;gap:8px;align-items:center;margin:8px 0 10px;min-width:0}.table-tools input[type=search]{flex:1 1 0;width:auto;min-width:0}.table-tools .btn{flex:0 0 auto;width:auto;min-width:68px;padding:0 12px;white-space:nowrap}.scroll{max-height:280px;overflow:auto;border:1px solid #dce5ef;border-radius:6px}.scroll.tall{max-height:520px}.scroll table{margin:0;border:0}.scroll th:first-child,.scroll td:first-child{border-left:0}.scroll th:last-child,.scroll td:last-child{border-right:0}.scroll th{position:sticky;top:0;z-index:1}
.pager{display:flex;gap:10px;align-items:center;justify-content:flex-end;margin-top:10px;color:var(--muted);font-size:13px}.pager a{color:var(--navy);font-weight:800;text-decoration:none}.pager .disabled{opacity:.4}
.pill{display:inline-block;border:1px solid #d5e1ec;background:#f7fafc;border-radius:999px;padding:3px 9px;color:#40566f}
.actions{display:flex;gap:8px;flex-wrap:wrap}.mini{height:32px;padding:0 10px}
.toolbar-forms{display:grid;grid-template-columns:repeat(3,minmax(240px,1fr));gap:12px;margin-bottom:14px}.inline-form{display:grid;grid-template-columns:minmax(0,1fr) auto;gap:8px}.cleanup-cell{min-width:150px;max-width:230px}.cleanup-cell-inner{display:flex;gap:8px;align-items:center;justify-content:space-between;text-align:left}.cleanup-summary{display:block;min-width:0;max-width:170px;color:var(--muted);font-size:12px;line-height:1.35;overflow-wrap:anywhere}.cleanup-dialog{width:min(680px,calc(100vw - 32px));max-height:min(760px,calc(100vh - 32px));padding:0;border:1px solid var(--line);border-radius:8px;color:var(--ink);box-shadow:0 18px 60px rgba(14,27,47,.26)}.cleanup-dialog::backdrop{background:rgba(14,27,47,.48)}.cleanup-dialog-form{display:flex;flex-direction:column;max-height:min(760px,calc(100vh - 32px))}.cleanup-dialog-head{display:flex;align-items:flex-start;justify-content:space-between;gap:12px;padding:16px 18px;border-bottom:1px solid var(--line);background:#f7f9fc}.cleanup-dialog-title{margin:0;font-size:19px}.cleanup-dialog-body{padding:16px 18px;overflow:auto;display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:12px;align-items:end}.cleanup-dialog-body .wide{grid-column:1/-1}.cleanup-dialog-body select,.cleanup-dialog-body input{width:100%}.cleanup-scopes{display:flex;gap:14px;align-items:center;flex-wrap:wrap;border:1px solid #dce5ef;border-radius:6px;padding:9px 10px}.cleanup-scopes label{display:flex;gap:5px;align-items:center}.cleanup-scopes input{width:auto;min-height:auto;height:auto}.cleanup-time{display:grid;grid-template-columns:minmax(90px,1fr) auto minmax(90px,1fr) auto;gap:8px;align-items:end}.cleanup-time-colon{font-size:22px;font-weight:800;padding-bottom:7px}.cleanup-time-clear{align-self:end}.cleanup-note{color:var(--muted);font-size:12px;line-height:1.45}.cleanup-dialog-actions{position:sticky;bottom:0;display:flex;justify-content:flex-end;gap:8px;padding:12px 18px;border-top:1px solid var(--line);background:#fff}.section-title{margin:4px 0 8px;font-size:15px;font-weight:800;color:#243852}.member-grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px;margin-bottom:14px}.member-form{border:1px solid #dce5ef;background:var(--soft);border-radius:8px;padding:12px;min-width:0}.member-form select{width:100%;margin-bottom:8px}.member-form select[multiple]{height:220px;min-height:220px;background:#fff}.group-name-list{max-width:760px;text-align:left;line-height:1.65}.permission-panels{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:14px;margin-bottom:14px}.permission-panel{border:1px solid #dce5ef;background:var(--soft);border-radius:8px;padding:12px;min-width:0}.permission-panel form{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;align-items:end}.permission-panel .btn{grid-column:1/-1;justify-self:start;min-width:180px}.permission-table td:first-child,.operator-name{text-align:left}.field-label{display:block;margin:0 0 5px;color:var(--muted);font-size:12px;font-weight:700}.field-stack{min-width:0}.field-stack select{width:100%}.field-stack.disabled{opacity:.45}
.watch-panel{max-height:620px;overflow:auto;border:1px solid #dce5ef;border-radius:8px;background:#fff}.watch-panel form{margin:0}.watch-head,.watch-row{display:grid;grid-template-columns:minmax(130px,.7fr) minmax(330px,1.6fr) minmax(150px,.7fr) repeat(2,88px) minmax(110px,.5fr) auto auto;gap:8px;align-items:center}.watch-head{position:sticky;top:0;z-index:1;background:#f4f7fb;font-weight:800;padding:10px;border-bottom:1px solid #dce5ef;text-align:center}.watch-row{padding:10px;border-bottom:1px solid #e7eef6}.watch-row:last-child{border-bottom:0}.watch-row code{word-break:break-all;color:#173f82}.watch-row .owner{font-weight:700}.watch-row .latest{color:var(--muted);font-size:12px}.watch-check{display:flex;gap:5px;align-items:center;justify-content:center}.watch-check input{min-height:auto}.watch-row .btn{height:34px;min-width:64px;padding:0 10px}.watch-empty{border:1px dashed #cbd8e8;border-radius:8px;padding:22px;text-align:center;color:var(--muted);background:var(--soft)}
@media(max-width:900px){.top{align-items:flex-start;flex-direction:column}.row,.row.two,.toolbar-forms,.member-grid,.permission-panels,.permission-panel form{grid-template-columns:1fr}.watch-head{display:none}.watch-row{grid-template-columns:1fr}.btn{width:100%}.table-tools{flex-wrap:nowrap}.table-tools input[type=search]{flex:1 1 0;width:0;min-width:0}.table-tools .btn{flex:0 0 auto;width:auto;min-width:68px}.cleanup-dialog-body{grid-template-columns:1fr}.cleanup-dialog-body .wide{grid-column:auto}.cleanup-dialog-actions .btn,.cleanup-time .btn{width:auto}.cleanup-cell{min-width:140px;max-width:190px}.cleanup-summary{max-width:125px}}
@media(max-width:420px){.wrap{padding:12px}.card{padding:14px}.table-tools{gap:6px}.table-tools .btn{min-width:64px;padding:0 10px}.cleanup-time{grid-template-columns:minmax(76px,1fr) auto minmax(76px,1fr)}.cleanup-time-clear{grid-column:1/-1;justify-self:start}.cleanup-dialog{width:calc(100vw - 16px);max-height:calc(100vh - 16px)}.cleanup-dialog-form{max-height:calc(100vh - 16px)}}
</style>
</head>
<body><main class="wrap">
<section class="top">
<div><div class="brand">Telegram 记账机器人</div><div class="title">后台管理</div><div class="sub">Go v{{.Version}} · {{.AdminRoleLabel}} · {{if or .CanManageGlobal .CanManageOperators .CanManageBroadcastPermissions .CanManageBroadcastGroups}}操作人、广播权限和地址监听{{else}}地址监听{{end}}</div></div>
<div class="actions"><a class="btn secondary" href="/admin">刷新</a><a class="btn secondary" href="/admin/logout">退出</a></div>
</section>
{{if .TokenUnset}}<div class="warn">当前没有配置 ADMIN_WEB_TOKEN，公网部署请先设置后台密码。</div>{{end}}
{{if .Message}}<div class="msg">{{.Message}}</div>{{end}}
<nav class="tabs" aria-label="后台模块">
{{if or .CanManageGlobal .CanManageBroadcastGroups}}
<button class="tab-btn active" type="button" data-admin-tab-target="groups">已保存群组</button>
<button class="tab-btn" type="button" data-admin-tab-target="broadcast">广播分组</button>
{{end}}
{{if or .CanManageOperators .CanManageBroadcastPermissions}}
<button class="tab-btn {{if not (or .CanManageGlobal .CanManageBroadcastGroups)}}active{{end}}" type="button" data-admin-tab-target="permissions">权限/操作人</button>
{{end}}
<button class="tab-btn {{if not (or .CanManageGlobal .CanManageOperators .CanManageBroadcastPermissions .CanManageBroadcastGroups)}}active{{end}}" type="button" data-admin-tab-target="watch">地址监听</button>
{{if .CanManageGlobal}}
<button class="tab-btn" type="button" data-admin-tab-target="replace">广播替换</button>
{{end}}
</nav>

<section class="grid">
{{if or .CanManageGlobal .CanManageBroadcastGroups}}
<div class="card tab-card active" data-admin-tab="groups">
<h2>已保存群组</h2>
<p class="hint">机器人被邀请进群，或群内有人发言后会自动保存群名；群改名后也会更新。</p>
<form class="table-tools" method="get" action="/admin"><input id="saved-group-search" name="groups_q" value="{{.GroupPager.Query}}" type="search" placeholder="搜索群名或群ID"><button class="btn mini" type="submit">搜索</button></form>
<div class="scroll tall"><table><thead><tr><th>群名</th><th>群ID</th><th>更新时间</th></tr></thead><tbody id="saved-group-rows">
{{range .Groups}}<tr data-search="{{chatLabel .}} {{.ChatID}}"><td>{{chatLabel .}}</td><td>{{.ChatID}}</td><td>{{.UpdatedAt.Format "2006-01-02 15:04"}}</td></tr>{{else}}<tr><td colspan="3">暂无群组</td></tr>{{end}}
</tbody></table></div>
<div class="pager"><span>{{.GroupPager.ItemFrom}}-{{.GroupPager.ItemTo}} / {{.GroupPager.Total}}</span>{{if .GroupPager.HasPrev}}<a href="{{.GroupPager.PrevURL}}">上一页</a>{{else}}<span class="disabled">上一页</span>{{end}}{{if .GroupPager.HasNext}}<a href="{{.GroupPager.NextURL}}">下一页</a>{{else}}<span class="disabled">下一页</span>{{end}}</div>
</div>

{{end}}
{{if .CanManageOperators}}
<div class="card tab-card {{if not .CanManageGlobal}}active{{end}}" data-admin-tab="permissions">
<h2>一级 / 下级操作人</h2>
<p class="hint">宿主和默认操作人来自环境配置；这里添加的是全局一级/下级操作人，可邀请机器人、进入后台和使用授权广播范围。页面不直接显示 UID，保存时仍按 UID 精确处理。</p>
<p class="hint">私聊清空只处理该操作人与机器人私聊，不会删除目标群投递或广播投递映射。</p>
<form method="post" action="/admin/operator/save" class="row">
<input name="user_id" placeholder="操作人 UID">
{{if .CanManageGlobal}}<select name="level"><option value="primary">一级操作人</option><option value="secondary">下级操作人</option></select><select name="parent_user_id"><option value="">创建下级时选择一级操作人</option>{{range .PrimaryOperators}}<option value="{{.UserID}}">{{operatorLabel .}}</option>{{end}}</select>{{else}}<input type="hidden" name="level" value="secondary"><span class="field-label">新增自己的下级操作人</span>{{end}}
<input name="remark" placeholder="备注，可选">
<button class="btn" type="submit">保存</button>
</form>
<form class="table-tools" method="get" action="/admin"><input name="operators_q" value="{{.OperatorPager.Query}}" type="search" placeholder="搜索备注、用户名或掩码"><button class="btn mini" type="submit">搜索</button></form>
<div class="scroll"><table><thead><tr><th>操作人</th><th>级别</th><th>授权来源</th><th>授权时间</th><th>私聊清空</th><th>状态</th><th>操作</th></tr></thead><tbody>
{{range .BOperators}}<tr><td>{{operatorLabel .}}</td><td>{{operatorLevelLabel .Level}}</td><td>{{operatorSourceLabel . $.OpLabels}}</td><td>{{adminTime .CreatedAt}}</td><td class="cleanup-cell"><div class="cleanup-cell-inner"><span class="cleanup-summary">{{cleanupSummary .}}</span>{{if $.CanManageGlobal}}<button class="btn mini secondary cleanup-open" type="button" data-cleanup-dialog="cleanup-dialog-{{.UserID}}">设置</button>{{end}}</div></td><td><span class="pill">{{.Status}}</span></td><td>{{if or (eq .Level "host") (eq .Level "default")}}环境配置{{else if canMutateOperator . $.CanManageGlobal $.AdminUserID}}{{if eq .Status "active"}}<form method="post" action="/admin/operator/disable"><input type="hidden" name="user_id" value="{{.UserID}}"><button class="btn mini" type="submit">禁用</button></form>{{else}}<form method="post" action="/admin/operator/save"><input type="hidden" name="user_id" value="{{.UserID}}"><input type="hidden" name="level" value="{{.Level}}"><input type="hidden" name="parent_user_id" value="{{.ParentUserID}}"><input type="hidden" name="remark" value="{{.Remark}}"><button class="btn mini" type="submit">启用</button></form>{{end}}{{else}}由一级操作人管理{{end}}</td></tr>{{else}}<tr><td colspan="7">暂无全局操作人</td></tr>{{end}}
</tbody></table></div>
{{if .CanManageGlobal}}{{range .BOperators}}
<dialog class="cleanup-dialog" id="cleanup-dialog-{{.UserID}}">
<form method="post" action="/admin/operator/cleanup" class="cleanup-dialog-form">
<input type="hidden" name="user_id" value="{{.UserID}}">
<div class="cleanup-dialog-head"><div><h3 class="cleanup-dialog-title">私聊自动清理</h3><div class="hint">{{operatorLabel .}} · 当前：{{cleanupSummary .}}</div></div></div>
<div class="cleanup-dialog-body">
<label><span class="field-label">总开关</span><select name="enabled"><option value="0" {{if not .PrivateCleanupEnabled}}selected{{end}}>关闭</option><option value="1" {{if .PrivateCleanupEnabled}}selected{{end}}>开启</option></select></label>
<label><span class="field-label">bot 提示消息</span><select name="bot_delete_after"><option value="0" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "0"}}selected{{end}}>不自动删</option><option value="30" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "30"}}selected{{end}}>30 秒后</option><option value="60" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "60"}}selected{{end}}>1 分钟后</option><option value="300" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "300"}}selected{{end}}>5 分钟后</option><option value="600" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "600"}}selected{{end}}>10 分钟后</option><option value="1800" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "1800"}}selected{{end}}>30 分钟后</option><option value="3600" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "3600"}}selected{{end}}>1 小时后</option><option value="custom" {{if eq (cleanupDelayPreset .PrivateCleanupBotDeleteAfterSeconds) "custom"}}selected{{end}}>自定义</option></select></label>
<label><span class="field-label">bot 自定义数值</span><input name="bot_delete_after_custom" value="{{cleanupDelayCustomValue .PrivateCleanupBotDeleteAfterSeconds}}" placeholder="数值"></label>
<label><span class="field-label">bot 自定义单位</span><select name="bot_delete_after_unit"><option value="minutes" {{if eq (cleanupDelayCustomUnit .PrivateCleanupBotDeleteAfterSeconds) "minutes"}}selected{{end}}>分钟</option><option value="seconds" {{if eq (cleanupDelayCustomUnit .PrivateCleanupBotDeleteAfterSeconds) "seconds"}}selected{{end}}>秒</option></select></label>
<label><span class="field-label">用户临时消息</span><select name="incoming_enabled"><option value="0" {{if not .PrivateCleanupIncomingEnabled}}selected{{end}}>不清理</option><option value="1" {{if .PrivateCleanupIncomingEnabled}}selected{{end}}>尝试清理</option></select></label>
<label><span class="field-label">用户消息多久删</span><select name="incoming_delete_after"><option value="0" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "0"}}selected{{end}}>仅每日兜底</option><option value="30" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "30"}}selected{{end}}>30 秒后</option><option value="60" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "60"}}selected{{end}}>1 分钟后</option><option value="300" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "300"}}selected{{end}}>5 分钟后</option><option value="600" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "600"}}selected{{end}}>10 分钟后</option><option value="1800" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "1800"}}selected{{end}}>30 分钟后</option><option value="3600" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "3600"}}selected{{end}}>1 小时后</option><option value="custom" {{if eq (cleanupDelayPreset .PrivateCleanupIncomingAfterSeconds) "custom"}}selected{{end}}>自定义</option></select></label>
<label><span class="field-label">用户消息自定义数值</span><input name="incoming_delete_after_custom" value="{{cleanupDelayCustomValue .PrivateCleanupIncomingAfterSeconds}}" placeholder="数值"></label>
<label><span class="field-label">用户消息自定义单位</span><select name="incoming_delete_after_unit"><option value="minutes" {{if eq (cleanupDelayCustomUnit .PrivateCleanupIncomingAfterSeconds) "minutes"}}selected{{end}}>分钟</option><option value="seconds" {{if eq (cleanupDelayCustomUnit .PrivateCleanupIncomingAfterSeconds) "seconds"}}selected{{end}}>秒</option></select></label>
<fieldset class="wide cleanup-scopes"><legend>清理范围</legend><label><input type="checkbox" name="scope" value="broadcast" {{if cleanupScopeEnabled .PrivateCleanupScope "broadcast"}}checked{{end}}>广播/单群</label><label><input type="checkbox" name="scope" value="quick_reply" {{if cleanupScopeEnabled .PrivateCleanupScope "quick_reply"}}checked{{end}}>快速回复</label><label><input type="checkbox" name="scope" value="menu" {{if cleanupScopeEnabled .PrivateCleanupScope "menu"}}checked{{end}}>菜单/后台提示</label></fieldset>
<div class="wide"><span class="field-label">每日兜底清空（北京时间）</span><div class="cleanup-time"><label><span class="field-label">小时</span><input type="number" name="cleanup_hour" min="0" max="23" step="1" inputmode="numeric" placeholder="00" value="{{cleanupTimeHour .PrivateCleanupTime}}"></label><span class="cleanup-time-colon">:</span><label><span class="field-label">分钟</span><input type="number" name="cleanup_minute" min="0" max="59" step="1" inputmode="numeric" placeholder="00" value="{{cleanupTimeMinute .PrivateCleanupTime}}"></label><button class="btn mini secondary cleanup-time-clear" type="button">清空/关闭时间</button></div></div>
<div class="cleanup-note wide">只处理该操作人与机器人私聊；不删除目标群投递，也不删除 broadcast_deliveries。两个时间框同时留空表示关闭每日兜底；用户素材/临时指令仅在开启后尝试清理，仍受 Telegram 删除限制。</div>
</div>
<div class="cleanup-dialog-actions"><button class="btn secondary cleanup-close" type="button">取消</button><button class="btn" type="submit">保存设置</button></div>
</form>
</dialog>
{{end}}{{end}}
<div class="pager"><span>{{.OperatorPager.ItemFrom}}-{{.OperatorPager.ItemTo}} / {{.OperatorPager.Total}}</span>{{if .OperatorPager.HasPrev}}<a href="{{.OperatorPager.PrevURL}}">上一页</a>{{else}}<span class="disabled">上一页</span>{{end}}{{if .OperatorPager.HasNext}}<a href="{{.OperatorPager.NextURL}}">下一页</a>{{else}}<span class="disabled">下一页</span>{{end}}</div>
</div>

{{end}}
{{if or .CanManageGlobal .CanManageBroadcastGroups}}
<div class="card wide tab-card" data-admin-tab="broadcast">
<h2>广播分组</h2>
<p class="hint">先创建分组，再用下方多选框批量添加或移除群组。页面显示群名，数据库仍用群 ID 去重。</p>
<form class="table-tools" method="get" action="/admin"><input name="broadcast_q" value="{{.BroadcastPager.Query}}" type="search" placeholder="搜索分组名或群名"><button class="btn mini" type="submit">搜索</button></form>
<div class="toolbar-forms">
<form method="post" action="/admin/group/save" class="inline-form">
<input name="name" placeholder="输入新分组名，例如 财务">
<button class="btn" type="submit">新建分组</button>
</form>
<form method="post" action="/admin/group/delete" class="inline-form">
<select name="name">{{range .BGroups}}{{if canManageBroadcastGroup . $.CanManageGlobal $.AdminUserID}}<option value="{{.Name}}">{{.Name}}</option>{{end}}{{end}}</select>
<button class="btn" type="submit">删除分组</button>
</form>
<form method="post" action="/admin/group/rename" class="inline-form">
<select name="old_name">{{range .BGroups}}{{if canManageBroadcastGroup . $.CanManageGlobal $.AdminUserID}}<option value="{{.Name}}">{{.Name}}</option>{{end}}{{end}}</select>
<input name="new_name" placeholder="新分组名">
<button class="btn" type="submit">改名并迁移授权</button>
</form>
</div>
<div class="member-grid">
<form method="post" action="/admin/group/add" class="member-form" data-mode="add">
<div class="section-title">添加群组到分组</div>
<label><span class="field-label">目标分组</span><select class="member-group-select" name="name">{{range .BGroups}}{{if canManageBroadcastGroup . $.CanManageGlobal $.AdminUserID}}<option value="{{.Name}}">{{.Name}}</option>{{end}}{{end}}</select></label>
<label><span class="field-label">选择要加入的群，可按 Ctrl/Shift 多选</span><select class="member-chat-select" name="chat_id" multiple>{{range .Groups}}<option value="{{.ChatID}}" data-groups="{{chatBroadcastGroups . $.BroadcastMemberships}}">{{chatLabel .}}</option>{{end}}</select></label>
<div class="hint member-empty" hidden>当前分组没有可选择的群。</div>
<button class="btn full" type="submit">添加到分组</button>
</form>
<form method="post" action="/admin/group/remove" class="member-form" data-mode="remove">
<div class="section-title">从分组移除群组</div>
<label><span class="field-label">目标分组</span><select class="member-group-select" name="name">{{range .BGroups}}{{if canManageBroadcastGroup . $.CanManageGlobal $.AdminUserID}}<option value="{{.Name}}">{{.Name}}</option>{{end}}{{end}}</select></label>
<label><span class="field-label">选择要移除的群，可按 Ctrl/Shift 多选</span><select class="member-chat-select" name="chat_id" multiple>{{range .Groups}}<option value="{{.ChatID}}" data-groups="{{chatBroadcastGroups . $.BroadcastMemberships}}">{{chatLabel .}}</option>{{end}}</select></label>
<div class="hint member-empty" hidden>当前分组没有可移除的群。</div>
<button class="btn full" type="submit">从分组移除</button>
</form>
</div>
<div class="scroll"><table><thead><tr><th>分组</th><th>权限</th><th>群数</th><th>群组</th></tr></thead><tbody>
{{range .BGroups}}<tr><td>{{.Name}}</td><td>{{if canManageBroadcastGroup . $.CanManageGlobal $.AdminUserID}}可管理{{else}}仅可使用{{end}}</td><td>{{len .ChatIDs}}</td><td>{{range $i,$n := .ChatNames}}{{if $i}}、{{end}}{{$n}}{{end}}</td></tr>{{else}}<tr><td colspan="4">暂无广播分组</td></tr>{{end}}
</tbody></table></div>
<div class="pager"><span>{{.BroadcastPager.ItemFrom}}-{{.BroadcastPager.ItemTo}} / {{.BroadcastPager.Total}}</span>{{if .BroadcastPager.HasPrev}}<a href="{{.BroadcastPager.PrevURL}}">上一页</a>{{else}}<span class="disabled">上一页</span>{{end}}{{if .BroadcastPager.HasNext}}<a href="{{.BroadcastPager.NextURL}}">下一页</a>{{else}}<span class="disabled">下一页</span>{{end}}</div>
</div>

{{end}}
{{if .CanManageBroadcastPermissions}}
<div class="card wide tab-card {{if not (or .CanManageGlobal .CanManageBroadcastGroups)}}active{{end}}" data-admin-tab="permissions">
<h2>广播权限</h2>
<p class="hint">宿主可管理全部授权；一级操作人只能把自己可使用的分组或直接拥有的单群授权给其他一级操作人和自己的下级。授权仅提供广播使用权，不转移分组管理权。</p>
<form class="table-tools" method="get" action="/admin"><input type="hidden" name="broadcast_q" value="{{.BroadcastPager.Query}}"><input name="groups_q" value="{{.GroupPager.Query}}" type="search" placeholder="搜索单群名称或群ID"><button class="btn mini" type="submit">筛选单群</button></form>
<form class="table-tools" method="get" action="/admin"><input type="hidden" name="groups_q" value="{{.GroupPager.Query}}"><input name="broadcast_q" value="{{.BroadcastPager.Query}}" type="search" placeholder="搜索广播分组"><button class="btn mini" type="submit">筛选分组</button></form>
<div class="permission-panels">
<div class="permission-panel">
<div class="section-title">授权广播目标</div>
<form method="post" action="/admin/permission/grant" class="permission-form" data-mode="grant">
<label class="field-stack"><span class="field-label">操作人</span><select class="permission-user-select" name="user_id">{{range .PermissionOperators}}{{if eq .Status "active"}}<option value="{{.UserID}}">{{operatorLabel .}}</option>{{end}}{{end}}</select></label>
<label class="field-stack"><span class="field-label">权限类型</span><select class="target-type" name="target"><option value="group">分组</option><option value="chat">单群</option></select></label>
<label class="field-stack target-group"><span class="field-label">选择分组</span><select class="permission-group-select" name="group_name">{{range .BGroups}}<option value="{{.Name}}" data-users="{{permissionGroupUsers .Name $.PermissionFilterData}}">{{.Name}}</option>{{end}}</select></label>
<label class="field-stack target-chat"><span class="field-label">选择单群</span><select class="permission-chat-select" name="chat_id">{{range .Groups}}<option value="{{.ChatID}}" data-users="{{permissionChatUsers . $.PermissionFilterData}}">{{chatLabel .}}</option>{{end}}</select></label>
<button class="btn" type="submit">授权</button>
</form>
</div>
<div class="permission-panel">
<div class="section-title">取消广播权限</div>
<form method="post" action="/admin/permission/revoke" class="permission-form" data-mode="revoke">
<label class="field-stack"><span class="field-label">操作人</span><select class="permission-user-select" name="user_id">{{range .PermissionOperators}}{{if eq .Status "active"}}<option value="{{.UserID}}">{{operatorLabel .}}</option>{{end}}{{end}}</select></label>
<label class="field-stack"><span class="field-label">权限类型</span><select class="target-type" name="target"><option value="group">分组</option><option value="chat">单群</option></select></label>
<label class="field-stack target-group"><span class="field-label">选择分组</span><select class="permission-group-select" name="group_name">{{range .BGroups}}<option value="{{.Name}}" data-users="{{permissionGroupUsers .Name $.PermissionFilterData}}">{{.Name}}</option>{{end}}</select></label>
<label class="field-stack target-chat"><span class="field-label">选择单群</span><select class="permission-chat-select" name="chat_id">{{range .Groups}}<option value="{{.ChatID}}" data-users="{{permissionChatUsers . $.PermissionFilterData}}">{{chatLabel .}}</option>{{end}}</select></label>
<button class="btn" type="submit">取消授权</button>
</form>
</div>
</div>
<div class="scroll"><table class="permission-table"><thead><tr><th>操作人</th><th>权限范围</th><th>授权来源</th></tr></thead><tbody>
{{range .Permissions}}<tr><td>{{permissionUserLabel . $.OpLabels}}</td><td>{{permissionTarget . $.ChatNames}}</td><td>{{grantorLabel . $.OpLabels}}</td></tr>{{else}}<tr><td colspan="3">暂无权限</td></tr>{{end}}
</tbody></table></div>
<form class="table-tools" method="get" action="/admin"><input name="permissions_q" value="{{.PermissionPager.Query}}" type="search" placeholder="搜索操作人、群名或授权来源"><button class="btn mini" type="submit">搜索</button></form>
<div class="pager"><span>{{.PermissionPager.ItemFrom}}-{{.PermissionPager.ItemTo}} / {{.PermissionPager.Total}}</span>{{if .PermissionPager.HasPrev}}<a href="{{.PermissionPager.PrevURL}}">上一页</a>{{else}}<span class="disabled">上一页</span>{{end}}{{if .PermissionPager.HasNext}}<a href="{{.PermissionPager.NextURL}}">下一页</a>{{else}}<span class="disabled">下一页</span>{{end}}</div>
</div>

{{end}}

<div class="card wide tab-card {{if not (or .CanManageGlobal .CanManageOperators .CanManageBroadcastPermissions)}}active{{end}}" data-admin-tab="watch">
<h2>地址监听</h2>
<p class="hint">宿主可查看全部监听地址；一级操作人和操作人只显示自己监听的地址。普通用户不能进入后台，只能在私聊机器人里管理自己的监听地址。</p>
{{if .WatchTargets}}
<div class="watch-panel">
<div class="watch-head"><div>所属用户</div><div>地址</div><div>备注</div><div>收入</div><div>支出</div><div>最小提醒</div><div>保存</div><div>删除</div></div>
{{range $i,$w := .WatchTargets}}
<form id="watch-save-{{$i}}" method="post" action="/admin/watch/save">
<input type="hidden" name="owner_user_id" value="{{$w.OwnerUserID}}">
<input type="hidden" name="address" value="{{$w.Address}}">
</form>
<form id="watch-remove-{{$i}}" method="post" action="/admin/watch/remove">
<input type="hidden" name="owner_user_id" value="{{$w.OwnerUserID}}">
<input type="hidden" name="address" value="{{$w.Address}}">
</form>
<div class="watch-row">
<div><div class="owner">{{watchOwnerLabel $w $.OpLabels}}</div><div class="latest">最近交易：{{watchLatestTime $w}}</div></div>
<code>{{$w.Address}}</code>
<input form="watch-save-{{$i}}" name="label" value="{{$w.Label}}" placeholder="备注">
<label class="watch-check"><input form="watch-save-{{$i}}" type="checkbox" name="watch_income" value="1" {{if $w.WatchIncome}}checked{{end}}>收入</label>
<label class="watch-check"><input form="watch-save-{{$i}}" type="checkbox" name="watch_expense" value="1" {{if $w.WatchExpense}}checked{{end}}>支出</label>
<input form="watch-save-{{$i}}" name="min_notify_amount" value="{{$w.MinNotifyAmount}}" placeholder="USDT">
<button class="btn mini" form="watch-save-{{$i}}" type="submit">保存</button>
<button class="btn mini secondary" form="watch-remove-{{$i}}" type="submit">删除</button>
</div>
{{end}}
</div>
{{else}}<div class="watch-empty">暂无监听地址</div>{{end}}
</div>

{{if .CanManageGlobal}}
<div class="card wide tab-card" data-admin-tab="replace">
<h2>广播替换</h2>
<p class="hint">开启后，仅对“单群发送”的投递消息生效：群成员回复该投递消息时，机器人会尝试把原投递消息替换为这里设置的固定图片/文字，然后再通知操作人。</p>
<form method="post" action="/admin/replace/save" enctype="multipart/form-data">
<div class="row">
<select name="enabled"><option value="0" {{if not .Replace.Enabled}}selected{{end}}>关闭，不替换原投递消息</option><option value="1" {{if .Replace.Enabled}}selected{{end}}>开启，回复后替换原投递消息</option></select>
<input type="file" name="image" accept="image/*">
<button class="btn" type="submit">保存替换设置</button>
</div>
<textarea class="full" name="text" rows="4" placeholder="固定替换文字，可作为图片说明">{{.Replace.Text}}</textarea>
<label class="hint"><input type="checkbox" name="remove_image" value="1"> 删除当前固定图片</label>
<p class="hint">当前状态：{{if .Replace.Enabled}}开启{{else}}关闭{{end}}。{{if .Replace.ImageName}}当前图片：{{.Replace.ImageName}}{{else}}当前没有固定图片{{end}}</p>
</form>
</div>
{{end}}
</section>
<script>
const adminTabs=Array.from(document.querySelectorAll('.tab-btn'));
const adminCards=Array.from(document.querySelectorAll('.tab-card'));
function setAdminTab(name){
  if(!adminTabs.length){return;}
  if(!adminTabs.some(function(btn){return btn.dataset.adminTabTarget===name;})){name=adminTabs[0].dataset.adminTabTarget;}
  adminTabs.forEach(function(btn){btn.classList.toggle('active',btn.dataset.adminTabTarget===name);});
  adminCards.forEach(function(card){card.classList.toggle('active',card.dataset.adminTab===name);});
  try{localStorage.setItem('ledger-admin-tab',name);}catch(e){}
  if(location.hash !== '#'+name){history.replaceState(null,'','#'+name);}
}
adminTabs.forEach(function(btn){btn.addEventListener('click',function(){setAdminTab(btn.dataset.adminTabTarget);});});
let initialTab=(location.hash || '').slice(1);
if(!initialTab){try{initialTab=localStorage.getItem('ledger-admin-tab') || '';}catch(e){}}
setAdminTab(initialTab || 'groups');
document.querySelectorAll('.cleanup-open').forEach(function(button){
  button.addEventListener('click',function(){
    const dialog=document.getElementById(button.dataset.cleanupDialog);
    if(dialog && typeof dialog.showModal==='function'){dialog.showModal();}
  });
});
document.querySelectorAll('.cleanup-dialog').forEach(function(dialog){
  dialog.querySelectorAll('.cleanup-close').forEach(function(button){button.addEventListener('click',function(){dialog.close();});});
  dialog.querySelectorAll('.cleanup-time-clear').forEach(function(button){
    button.addEventListener('click',function(){
      const form=button.closest('form');
      if(!form){return;}
      const hour=form.querySelector('[name="cleanup_hour"]');
      const minute=form.querySelector('[name="cleanup_minute"]');
      if(hour){hour.value='';}
      if(minute){minute.value='';}
      if(hour){hour.focus();}
    });
  });
  dialog.addEventListener('click',function(event){if(event.target===dialog){dialog.close();}});
});
const groupSearch=document.getElementById('saved-group-search');
if(groupSearch){
  groupSearch.addEventListener('input',function(){
    const q=this.value.trim().toLowerCase();
    document.querySelectorAll('#saved-group-rows tr[data-search]').forEach(function(row){
      row.style.display=row.dataset.search.toLowerCase().includes(q)?'':'none';
    });
  });
}
document.querySelectorAll('.permission-form').forEach(function(form){
  const mode=form.dataset.mode || 'grant';
  const user=form.querySelector('.permission-user-select');
  const type=form.querySelector('.target-type');
  const group=form.querySelector('.target-group');
  const chat=form.querySelector('.target-chat');
  const groupSelect=form.querySelector('.permission-group-select');
  const chatSelect=form.querySelector('.permission-chat-select');
  function optionUsers(option){
    try{return JSON.parse(option.dataset.users || '[]').map(String);}catch(e){return [];}
  }
  function syncPermissionSelect(select, userID){
    if(!select){return 0;}
    let visible=0;
    let firstVisible=null;
    let selectedVisible=false;
    Array.from(select.options).forEach(function(option){
      const hasPermission=optionUsers(option).includes(String(userID || ''));
      const show=mode==='revoke'?hasPermission:!hasPermission;
      option.hidden=!show;
      option.disabled=!show;
      if(show){
        visible++;
        if(!firstVisible){firstVisible=option;}
        if(option.selected){selectedVisible=true;}
      }else{
        option.selected=false;
      }
    });
    if(!selectedVisible && firstVisible){firstVisible.selected=true;}
    return visible;
  }
  function syncTarget(){
    const useChat=type && type.value==='chat';
    const userID=user ? user.value : '';
    const groupVisible=syncPermissionSelect(groupSelect,userID);
    const chatVisible=syncPermissionSelect(chatSelect,userID);
    if(group){
      group.classList.toggle('disabled', useChat || groupVisible===0);
      if(groupSelect){groupSelect.disabled=useChat || groupVisible===0;}
    }
    if(chat){
      chat.classList.toggle('disabled', !useChat || chatVisible===0);
      if(chatSelect){chatSelect.disabled=!useChat || chatVisible===0;}
    }
  }
  if(type){type.addEventListener('change',syncTarget);}
  if(user){user.addEventListener('change',syncTarget);}
  syncTarget();
});
document.querySelectorAll('.member-form').forEach(function(form){
  const mode=form.dataset.mode || 'add';
  const groupSelect=form.querySelector('.member-group-select');
  const chatSelect=form.querySelector('.member-chat-select');
  const empty=form.querySelector('.member-empty');
  function optionGroups(option){
    try{return JSON.parse(option.dataset.groups || '[]');}catch(e){return [];}
  }
  function syncMemberOptions(){
    if(!groupSelect || !chatSelect){return;}
    const groupName=groupSelect.value || '';
    let visible=0;
    Array.from(chatSelect.options).forEach(function(option){
      const inGroup=optionGroups(option).includes(groupName);
      const show=mode==='remove'?inGroup:!inGroup;
      option.hidden=!show;
      option.disabled=!show;
      if(!show){option.selected=false;}
      if(show){visible++;}
    });
    if(empty){empty.hidden=visible!==0;}
  }
  if(groupSelect){groupSelect.addEventListener('change',syncMemberOptions);}
  syncMemberOptions();
});
</script>
</main></body></html>`

const billHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>完整账单</title>
<style>
:root{--bg:#eaf1f7;--panel:#fff;--panel-soft:#f5f8fb;--line:#c8d6e6;--line-soft:#dfe8f2;--text:#0e1b2f;--muted:#5b6f88;--gold:#b87916;--gold-soft:#fbf2dc;--blue:#1f5fae;--blue-dark:#143f82;--blue-soft:#edf5ff;--shadow:0 8px 24px rgba(36,77,114,.07)}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font-family:Arial,"Microsoft YaHei","PingFang SC",sans-serif;font-size:14px;line-height:1.5}a{color:var(--blue);text-decoration:none}a:hover{color:var(--gold);text-decoration:underline}
.content-wrapper{min-height:100vh;width:100%;max-width:1280px;margin:0 auto;padding:28px 32px 36px}.container{width:100%;margin:0 auto}.content{min-height:250px;display:grid;grid-template-columns:minmax(0,1fr);gap:16px}
.bill-toolbar,.bill-search,.box{background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:var(--shadow)}.bill-toolbar{display:flex;justify-content:space-between;gap:16px;align-items:flex-start;padding:4px 0 16px;margin-bottom:8px;background:transparent;border:0;border-radius:0;box-shadow:none}
.bill-heading{display:flex;flex-direction:column;gap:3px;min-width:0}.bill-heading .brand{color:var(--gold);font-weight:800}.bill-heading h1{margin:0;font-size:28px;font-weight:800;line-height:1.25;overflow-wrap:anywhere}.bill-heading p{margin:0;color:#536782}
.toolbar-actions{display:flex;flex-wrap:wrap;gap:8px;justify-content:flex-end;align-items:center}.btn{display:inline-flex;align-items:center;justify-content:center;min-height:34px;padding:7px 12px;border:1px solid var(--line);border-radius:6px;background:#fff;color:var(--text);font-weight:600;white-space:nowrap}.btn:hover{background:var(--blue-soft);text-decoration:none}.btn.primary{border-color:var(--blue);background:var(--blue);color:#fff}.btn.primary:hover{background:var(--blue-dark);color:#fff}
.history-menu{position:relative;display:inline-flex;align-items:center;min-height:34px;z-index:5}.history-trigger{cursor:pointer;font-family:inherit;font-size:14px;font-weight:600;line-height:1.2}.history-dropdown{display:none;position:absolute;top:40px;left:0;min-width:92px;max-height:520px;overflow-y:auto;padding:6px 0;background:#fff;border:1px solid var(--line);border-radius:4px;box-shadow:0 12px 28px rgba(20,42,75,.16)}.history-menu:hover .history-dropdown,.history-menu:focus-within .history-dropdown{display:block}.history-dropdown a,.history-empty{display:block;padding:3px 14px;line-height:22px;color:var(--muted);white-space:nowrap}.history-dropdown a:hover{background:var(--blue-soft);color:var(--blue);text-decoration:none}.history-dropdown a.active{color:var(--gold);font-weight:700;background:var(--gold-soft)}
.summary-grid{display:grid;grid-template-columns:repeat(6,minmax(0,1fr));gap:10px;margin-bottom:18px}.summary-card{min-height:78px;padding:15px 16px;background:var(--panel);border:1px solid var(--line);border-radius:8px;box-shadow:var(--shadow)}.summary-card span{display:block;margin-bottom:8px;color:var(--muted);font-size:13px}.summary-card strong{display:block;color:var(--text);font-size:19px;line-height:1.25;overflow-wrap:anywhere}
.bill-search{display:flex;justify-content:center;gap:8px;width:100%;padding:12px 14px;margin:0 0 26px}.bill-search input[type=text]{flex:1 1 360px;min-width:0;height:34px;border-radius:6px;border:1px solid var(--line);padding:0 10px;background:#fff}.bill-search select{flex:0 0 130px;height:34px;border-radius:6px;border:1px solid var(--line);background:#fff}.bill-search button{flex:0 0 86px;height:34px;border-radius:6px;background:var(--blue);color:#fff;border:0;cursor:pointer;font-weight:700}.bill-search button:hover{background:var(--blue-dark)}
.panel{width:100%;margin:0;padding:0;background:transparent;border:0;box-shadow:none}.box{margin:0;padding:20px;width:100%;border-top:5px solid #efdca9}.box-primary{border-left:1px solid var(--line)}.box-header{display:block;padding-bottom:12px;border-bottom:1px solid var(--line-soft);margin-bottom:8px}.box-title{display:inline-block;margin:0;font-size:22px;font-weight:bold;line-height:1.2}.box-body{padding:0}.table-wrap{overflow-x:auto}
table{width:100%;max-width:100%;border-collapse:collapse;table-layout:fixed}td{padding:10px 8px!important;overflow-wrap:anywhere;white-space:normal;text-align:center;vertical-align:middle;border:1px solid var(--line-soft)}.records thead td,.records .table-head td{font-weight:800;color:#172336;background:var(--panel-soft)}.records td:first-child{white-space:nowrap}.records tbody tr:hover td{background:#fbfdff}.col-time{width:14%}.col-amount{width:24%}.col-rate{width:22%}.col-actor{width:24%}.col-note{width:16%}.copyable{cursor:pointer;border-bottom:1px dotted #94a3b8}.message-link{color:#1677d2}.empty{color:var(--muted);text-align:center;padding:18px 0!important}
.footer-note{display:flex;gap:18px;flex-wrap:wrap;margin-top:14px;color:var(--muted);font-size:14px}.footer-note strong{color:var(--text)}
@media(max-width:920px){.content-wrapper{padding:20px 16px 28px}.bill-toolbar{align-items:stretch;flex-direction:column}.toolbar-actions{justify-content:flex-start}.summary-grid{grid-template-columns:repeat(2,minmax(0,1fr))}}
@media(max-width:640px){body{font-size:13px}.bill-heading h1{font-size:24px}.summary-grid{grid-template-columns:1fr}.summary-card{min-height:68px}.box{padding:14px 10px;margin-bottom:12px}.box-title{font-size:18px}.bill-search{flex-direction:column;margin-bottom:16px}.bill-search input[type=text],.bill-search select,.bill-search button{width:100%;flex:auto}.toolbar-actions .btn,.history-menu,.history-trigger{width:100%}.history-dropdown{left:0;right:0}.records{min-width:760px}}
</style>
</head>
<body><main class="content-wrapper"><div class="container">
<section class="bill-toolbar">
<div class="bill-heading"><div class="brand">Telegram 记账机器人</div><h1>{{.Group.Title}}</h1><p>群 ID：{{.Group.ChatID}} · {{.TitleDay}} · 北京时间</p></div>
<nav class="toolbar-actions">
<a class="btn" href="{{.TodayPath}}">今日</a>
<a class="btn" href="{{.PrevPath}}">上一天</a>
<a class="btn" href="{{.NextPath}}">下一天</a>
{{if .NewerPagePath}}<a class="btn" href="{{.NewerPagePath}}">上一页</a>{{end}}
{{if .OlderPagePath}}<a class="btn" href="{{.OlderPagePath}}">下一页</a>{{end}}
<span class="history-menu"><button type="button" class="btn history-trigger">历史账单⌄</button><span class="history-dropdown">{{range .HistoryLinks}}<a class="{{if .Active}}active{{end}}" href="{{.URL}}">{{.Label}}</a>{{else}}<span class="history-empty">无历史账单</span>{{end}}</span></span>
<a class="btn" href="{{.DownloadPath}}">下载账单</a>
</nav>
</section>
<section class="summary-grid">
<div class="summary-card"><span>总入款</span><strong>{{.Summary.TotalDepositCNY}} / {{.Summary.TotalDepositGrossUSDT}}U</strong></div>
<div class="summary-card"><span>汇率</span><strong>{{.Summary.ExchangeRate}}</strong></div>
<div class="summary-card"><span>交易费率</span><strong>{{.Summary.FeeRate}}%</strong></div>
<div class="summary-card"><span>应下发</span><strong>{{.Summary.TotalDepositNetUSDT}}U</strong></div>
<div class="summary-card"><span>已下发</span><strong>{{.Summary.TotalPayoutUSDT}}U</strong></div>
<div class="summary-card"><span>余额</span><strong>{{.Summary.BalanceUSDT}}U</strong></div>
</section>
<form class="bill-search" method="get" action="/b/{{.Group.ChatID}}/{{.DayKey}}">
<input type="text" name="q" value="{{.Query}}" placeholder="输入您要查询的名字或者备注关键词">
<select name="field">
<option value="all" {{if eq .Field "all"}}selected{{end}}>全部字段</option>
<option value="subject" {{if eq .Field "subject"}}selected{{end}}>按标记人</option>
<option value="actor" {{if eq .Field "actor"}}selected{{end}}>按操作人</option>
<option value="remark" {{if eq .Field "remark"}}selected{{end}}>按备注</option>
<option value="amount" {{if eq .Field "amount"}}selected{{end}}>按金额</option>
</select>
<button type="submit">搜索</button>
</form>
<section class="content">
<section class="panel"><div class="box box-primary"><div class="box-header"><h3 class="box-title">入款 (<span>{{.Summary.DepositCount}}</span>笔)</h3></div><div class="box-body"><div class="table-wrap"><table class="records"><colgroup><col class="col-time"><col class="col-amount"><col class="col-rate"><col class="col-actor"><col class="col-note"></colgroup><thead><tr><td>时间</td><td>金额</td><td>标记人</td><td>操作人</td><td>备注</td></tr></thead><tbody>{{range .Summary.Deposits}}<tr><td>{{billTime .CreatedAt}}</td><td>{{billAmountHTML .}}</td><td>{{recordSubjectHTML .}}</td><td>{{recordActor .}}</td><td>{{.Remark}}</td></tr>{{else}}<tr><td colspan="5" class="empty">暂无记录</td></tr>{{end}}</tbody></table></div></div></div></section>
<section class="panel"><div class="box box-primary"><div class="box-header"><h3 class="box-title">下发 (<span>{{.Summary.PayoutCount}}</span>笔)</h3></div><div class="box-body"><div class="table-wrap"><table class="records"><colgroup><col class="col-time"><col class="col-amount"><col class="col-rate"><col class="col-actor"><col class="col-note"></colgroup><thead><tr><td>时间</td><td>金额</td><td>标记人</td><td>操作人</td><td>备注</td></tr></thead><tbody>{{range .Summary.Payouts}}<tr><td>{{billTime .CreatedAt}}</td><td>{{billAmountHTML .}}</td><td>{{recordSubjectHTML .}}</td><td>{{recordActor .}}</td><td>{{.Remark}}</td></tr>{{else}}<tr><td colspan="5" class="empty">暂无记录</td></tr>{{end}}</tbody></table></div></div></div></section>
<section class="panel"><div class="box box-primary"><div class="box-header"><h3 class="box-title">统计（按标记人） ({{len .Summary.SubjectStats}} 人)</h3></div><div class="box-body"><div class="table-wrap"><table class="records"><tbody><tr class="table-head"><td>用户名</td><td>入款</td><td>已下发</td><td>未下发</td></tr>{{range .Summary.SubjectStats}}<tr><td>{{.Name}} ({{.Count}} 笔)</td><td><span class="copyable">{{.InCNY}}</span>/<span class="copyable">{{.InUSDT}}</span>U</td><td><span class="copyable">{{.OutCNY}}</span>/<span class="copyable">{{.OutUSDT}}</span>U</td><td><span class="copyable">{{.BalanceCNY}}</span>/<span class="copyable">{{.BalanceUSDT}}</span>U</td></tr>{{else}}<tr><td colspan="4" class="empty">暂无统计</td></tr>{{end}}</tbody></table></div></div></div></section>
<section class="panel"><div class="box box-primary"><div class="box-header"><h3 class="box-title">统计（按操作人） ({{len .Summary.ActorStats}} 人)</h3></div><div class="box-body"><div class="table-wrap"><table class="records"><tbody><tr class="table-head"><td>用户名</td><td>入款</td><td>已下发</td><td>未下发</td></tr>{{range .Summary.ActorStats}}<tr><td>{{.Name}} ({{.Count}} 笔)</td><td><span class="copyable">{{.InCNY}}</span>/<span class="copyable">{{.InUSDT}}</span>U</td><td><span class="copyable">{{.OutCNY}}</span>/<span class="copyable">{{.OutUSDT}}</span>U</td><td><span class="copyable">{{.BalanceCNY}}</span>/<span class="copyable">{{.BalanceUSDT}}</span>U</td></tr>{{else}}<tr><td colspan="4" class="empty">暂无统计</td></tr>{{end}}</tbody></table></div></div></div></section>
<section class="panel"><div class="box box-primary"><div class="box-header"><h3 class="box-title">统计（按备注） ({{len .Summary.RemarkStats}} 人)</h3></div><div class="box-body"><div class="table-wrap"><table class="records"><tbody><tr class="table-head"><td>用户名</td><td>入款</td><td>已下发</td><td>未下发</td></tr>{{range .Summary.RemarkStats}}<tr><td>{{.Name}} ({{.Count}} 笔)</td><td><span class="copyable">{{.InCNY}}</span>/<span class="copyable">{{.InUSDT}}</span>U</td><td><span class="copyable">{{.OutCNY}}</span>/<span class="copyable">{{.OutUSDT}}</span>U</td><td><span class="copyable">{{.BalanceCNY}}</span>/<span class="copyable">{{.BalanceUSDT}}</span>U</td></tr>{{else}}<tr><td colspan="4" class="empty">暂无统计</td></tr>{{end}}</tbody></table></div></div></div></section>
<section class="panel"><div class="box box-primary"><div class="box-header"><h3 class="box-title">统计（按汇率分类）</h3></div><div class="box-body"><div class="table-wrap"><table class="records"><tbody><tr class="table-head"><td>汇率</td><td>入款</td><td>换算U</td></tr>{{range .Summary.RateStats}}<tr><td>{{.Rate}}</td><td>{{.AmountCNY}}</td><td>{{.AmountUSDT}} U</td></tr>{{else}}<tr><td colspan="3" class="empty">暂无统计</td></tr>{{end}}</tbody></table></div></div></div></section>
<div class="footer-note"><span>总入款：<strong>{{.Summary.TotalDepositCNY}} / {{.Summary.TotalDepositGrossUSDT}}U</strong></span><span>汇率：<strong>{{.Summary.ExchangeRate}}</strong></span><span>交易费率：<strong>{{.Summary.FeeRate}}%</strong></span></div>
</section>
</div></main></body></html>`
