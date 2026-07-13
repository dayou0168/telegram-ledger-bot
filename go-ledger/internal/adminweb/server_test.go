package adminweb

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminauth"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/xuri/excelize/v2"
)

func TestBillExchangeRateDisplay(t *testing.T) {
	group := storage.Group{
		DepositExchangeRate: "6.63000000",
		ExchangeRateSource:  "支付宝",
		ExchangeRateRank:    1,
		ExchangeRateOffset:  "-0.1000",
	}
	if got, want := billExchangeRateDisplay(group), "支付宝1档 下浮0.1"; got != want {
		t.Fatalf("billExchangeRateDisplay = %q, want %q", got, want)
	}
	group.ExchangeRateOffset = "0"
	if got, want := billExchangeRateDisplay(group), "支付宝1档"; got != want {
		t.Fatalf("billExchangeRateDisplay zero offset = %q, want %q", got, want)
	}
	group.ExchangeRateSource = ""
	group.ExchangeRateRank = 0
	if got, want := billExchangeRateDisplay(group), "6.63"; got != want {
		t.Fatalf("billExchangeRateDisplay manual = %q, want %q", got, want)
	}
}

func TestAddressWatchAdminDoesNotAdvertiseUnsupportedTRXNotifications(t *testing.T) {
	if strings.Contains(adminHTML, ">TRX<") || strings.Contains(adminHTML, "name=\"notify_trx\"") {
		t.Fatal("address watch admin still exposes unsupported TRX notification controls")
	}
}

func TestSummarizeBillIncludesSubjectAndRateStats(t *testing.T) {
	group := storage.Group{DepositExchangeRate: "10", FeeRate: "3"}
	records := []storage.Record{
		{
			Kind:        "deposit",
			Currency:    "CNY",
			Amount:      "100",
			Rate:        "10",
			FeeRate:     "3",
			ResultUSDT:  "9.7",
			SubjectName: "新一",
			ActorName:   "阿泽",
			Remark:      "测试",
		},
		{
			Kind:        "payout",
			Currency:    "USDT",
			Amount:      "2",
			Rate:        "10",
			ResultUSDT:  "2",
			SubjectName: "新一",
			ActorName:   "阿泽",
		},
	}
	summary := summarizeBill(group, records)
	if summary.TotalDepositCNY != "100" || summary.TotalDepositGrossUSDT != "10" || summary.TotalDepositNetUSDT != "9.7" {
		t.Fatalf("unexpected deposit totals: %+v", summary)
	}
	if len(summary.SubjectStats) != 1 || summary.SubjectStats[0].Name != "新一" || summary.SubjectStats[0].BalanceUSDT != "7.7" {
		t.Fatalf("unexpected subject stats: %+v", summary.SubjectStats)
	}
	if len(summary.RateStats) != 1 || summary.RateStats[0].Rate != "10" {
		t.Fatalf("unexpected rate stats: %+v", summary.RateStats)
	}
}

func TestBillTemplateRendersReferenceStyleSections(t *testing.T) {
	day := "2026-07-06"
	data := billData{
		Group:        storage.Group{ChatID: -1001, Title: "测试群"},
		DayKey:       day,
		TitleDay:     day,
		Summary:      summarizeBill(storage.Group{DepositExchangeRate: "10"}, nil),
		HistoryLinks: []billHistoryLink{{DayKey: day, Label: "07-06", URL: "/b/-1001/20260706", Active: true}},
		TodayPath:    "/b/-1001/20260706",
		PrevPath:     "/b/-1001/20260705",
		NextPath:     "/b/-1001/20260707",
		DownloadPath: "/b/-1001/20260706/download",
		Field:        "all",
	}
	var buf bytes.Buffer
	if err := billTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	wants := []string{"历史账单⌄", "上一天", "下一天", "统计（按标记人）", "统计（按操作人）", "统计（按备注）", "统计（按汇率分类）"}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Fatalf("bill template missing %q", want)
		}
	}
}

func TestBillHistoryTriggerUsesButtonFont(t *testing.T) {
	if strings.Contains(billHTML, ".history-trigger{font:inherit") {
		t.Fatal("history trigger should not reset button font styling")
	}
	if !strings.Contains(billHTML, ".history-trigger{cursor:pointer;font-family:inherit;font-size:14px;font-weight:600") {
		t.Fatal("history trigger should match toolbar button typography")
	}
}

func TestBillRecordLinksOnlyAmountAndSubject(t *testing.T) {
	record := storage.Record{
		ChatID:          -1003720457420,
		SourceMessageID: 1234,
		Amount:          "666",
		Rate:            "10",
		FeeRate:         "0",
		ResultUSDT:      "66.6",
		Currency:        "CNY",
		Kind:            "deposit",
		SubjectName:     "新一",
	}
	amount := string(billAmountHTML(record))
	if !strings.Contains(amount, `>666</a>/10=66.6U`) || strings.Count(amount, `href=`) != 1 {
		t.Fatalf("amount link should exclude formula remainder: %s", amount)
	}
	subject := string(recordSubjectHTML(record))
	if !strings.Contains(subject, `>新一</a>`) || !strings.Contains(subject, `https://t.me/c/3720457420/1234`) {
		t.Fatalf("subject link = %s", subject)
	}
	record.SourceMessageID = 0
	if strings.Contains(string(billAmountHTML(record)), "<a") || strings.Contains(string(recordSubjectHTML(record)), "<a") {
		t.Fatal("record without a valid source message must render as plain text")
	}
}

func TestBillCursorPathPreservesFilter(t *testing.T) {
	got := billCursorPath(-1001, "2026-07-12", "subject", "新一", "before", 900)
	for _, want := range []string{"/b/-1001/20260712?", "before=900", "field=subject", "q="} {
		if !strings.Contains(got, want) {
			t.Fatalf("cursor path %q missing %q", got, want)
		}
	}
}

func TestWriteBillXLSXConsumesBatches(t *testing.T) {
	var calls []string
	walker := func(kind string, visit func([]storage.Record) error) error {
		calls = append(calls, kind)
		if kind == "payout" {
			return nil
		}
		return visit([]storage.Record{{ID: 1, Kind: "deposit", Currency: "CNY", Amount: "10", Rate: "1", ResultUSDT: "10"}})
	}
	var output bytes.Buffer
	if err := writeBillXLSX(storage.Group{Title: "test", DepositExchangeRate: "1"}, "2026-07-12", walker, &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 || len(calls) != 3 || calls[0] != "" || calls[1] != "deposit" || calls[2] != "payout" {
		t.Fatalf("walker calls = %v, output bytes = %d", calls, output.Len())
	}
	file, err := excelize.OpenReader(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatalf("open streamed workbook: %v", err)
	}
	defer func() { _ = file.Close() }()
	if value, err := file.GetCellValue("账单", "A3"); err != nil || value != "入款：1笔" {
		t.Fatalf("streamed workbook A3 = %q, err = %v", value, err)
	}
}

func TestAdminGlobalManagementIsHostOnly(t *testing.T) {
	s := New(config.Config{HostUserID: 1001, DefaultOperatorIDs: map[int64]struct{}{2002: {}}}, nil)
	if !s.adminCanManageGlobal(adminauth.Session{UserID: 1001, Role: adminauth.RoleHost}) {
		t.Fatal("host should manage global admin modules")
	}
	if s.adminCanManageGlobal(adminauth.Session{UserID: 2002, Role: adminauth.RoleDefaultOperator}) {
		t.Fatal("default operator should not receive global backend management")
	}
	if s.adminCanManageGlobal(adminauth.Session{UserID: 3003, Role: adminauth.RoleOperator}) {
		t.Fatal("operator should not receive global backend management")
	}
}

func TestAdminSessionSecretRequiresAdminWebToken(t *testing.T) {
	withoutToken := &Server{cfg: config.Config{TelegramBotToken: "telegram-secret"}}
	if got := withoutToken.adminSessionSecret(); got != "" {
		t.Fatalf("admin session secret without ADMIN_WEB_TOKEN = %q, want empty", got)
	}
	withToken := &Server{cfg: config.Config{AdminWebToken: " admin-secret "}}
	if got := withToken.adminSessionSecret(); got != "admin-secret" {
		t.Fatalf("admin session secret = %q, want trimmed ADMIN_WEB_TOKEN", got)
	}
}

func TestLoginTemplateExplainsInvalidShortcutCanUsePassword(t *testing.T) {
	rec := httptest.NewRecorder()
	renderLogin(rec, false, "快捷登录链接无效或已过期，请输入后台密码登录")
	html := rec.Body.String()
	if !strings.Contains(html, "快捷登录链接无效或已过期，请输入后台密码登录") {
		t.Fatal("login page should explain invalid shortcut login can fall back to password")
	}
	if !strings.Contains(html, `name="password"`) || !strings.Contains(html, "进入后台") {
		t.Fatal("login page should keep password login available")
	}
}

func TestLoginTemplateRendersShortcutSubmitWithoutHidingPassword(t *testing.T) {
	rec := httptest.NewRecorder()
	renderLoginWithTicket(rec, false, "快捷登录链接有效，点击下方按钮进入后台。", "ticket-value")
	html := rec.Body.String()
	for _, want := range []string{
		`method="post" action="/admin/login"`,
		`name="ticket" value="ticket-value"`,
		"使用快捷登录进入后台",
		`name="password"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("login page missing %q", want)
		}
	}
}

func TestAdminCookieSecureFlag(t *testing.T) {
	rec := httptest.NewRecorder()
	s := &Server{cfg: config.Config{AdminWebToken: "secret", AdminWebCookieSecure: true}}
	s.setAdminCookie(rec, adminauth.Session{UserID: 1, Role: adminauth.RoleHost, ExpiresAt: time.Now().Add(time.Hour)})
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies len = %d, want 1", len(cookies))
	}
	if !cookies[0].Secure {
		t.Fatal("admin cookie should honor AdminWebCookieSecure=true")
	}
}

func TestRequirePostRejectsGet(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/operator/disable", nil)
	if requirePost(rec, req) {
		t.Fatal("GET should not satisfy requirePost")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestParseBroadcastPermissionFormValidatesInput(t *testing.T) {
	tests := []struct {
		name string
		form string
		ok   bool
	}{
		{name: "valid chat", form: "user_id=123&target=chat&chat_id=-1001", ok: true},
		{name: "valid group", form: "user_id=123&target=group&group_name=alpha", ok: true},
		{name: "zero user", form: "user_id=0&target=chat&chat_id=-1001"},
		{name: "empty chat", form: "user_id=123&target=chat&chat_id=0"},
		{name: "empty group", form: "user_id=123&target=group&group_name="},
		{name: "bad target", form: "user_id=123&target=all&chat_id=-1001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/admin/permission/grant", strings.NewReader(tt.form))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			_, _, _, _, _, ok := parseBroadcastPermissionForm(req)
			if ok != tt.ok {
				t.Fatalf("parseBroadcastPermissionForm ok = %v, want %v", ok, tt.ok)
			}
		})
	}
}

type testPermissionInvalidator struct {
	broadcastUserID int64
	allPermissions  bool
	watchTargets    bool
}

func (i *testPermissionInvalidator) InvalidateBroadcastPermission(userID int64) {
	i.broadcastUserID = userID
}

func (i *testPermissionInvalidator) InvalidateAllPermissionCaches() {
	i.allPermissions = true
}

func (i *testPermissionInvalidator) InvalidateWatchTargets() {
	i.watchTargets = true
}

func TestServerPermissionInvalidatorHooks(t *testing.T) {
	invalidator := &testPermissionInvalidator{}
	s := New(config.Config{}, nil, invalidator)

	s.invalidateBroadcastPermission(2002)
	s.invalidateAllPermissionCaches()
	s.invalidateWatchTargets()

	if invalidator.broadcastUserID != 2002 {
		t.Fatalf("broadcast invalidation user = %d, want 2002", invalidator.broadcastUserID)
	}
	if !invalidator.allPermissions {
		t.Fatal("all permission caches invalidation hook was not called")
	}
	if !invalidator.watchTargets {
		t.Fatal("watch target invalidation hook was not called")
	}
}

func TestNormalizeCleanupTime(t *testing.T) {
	for raw, want := range map[string]string{
		"8:05":  "08:05",
		"08.05": "08:05",
		"23:59": "23:59",
	} {
		got, ok := normalizeCleanupTime(raw)
		if !ok || got != want {
			t.Fatalf("normalizeCleanupTime(%q) = %q, %v; want %q, true", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"24:00", "12:60", "1200", "abc"} {
		if got, ok := normalizeCleanupTime(raw); ok {
			t.Fatalf("normalizeCleanupTime(%q) = %q, true; want invalid", raw, got)
		}
	}
}

func TestNormalizeCleanupDelay(t *testing.T) {
	cases := []struct {
		preset string
		custom string
		unit   string
		want   int
	}{
		{preset: "0", want: 0},
		{preset: "30", want: 30},
		{preset: "3600", want: 3600},
		{preset: "custom", custom: "45", unit: "seconds", want: 45},
		{preset: "custom", custom: "3", unit: "minutes", want: 180},
	}
	for _, tc := range cases {
		got, ok := normalizeCleanupDelay(tc.preset, tc.custom, tc.unit)
		if !ok || got != tc.want {
			t.Fatalf("normalizeCleanupDelay(%q,%q,%q) = %d,%v; want %d,true", tc.preset, tc.custom, tc.unit, got, ok, tc.want)
		}
	}
	for _, tc := range []struct {
		preset string
		custom string
		unit   string
	}{
		{preset: "abc"},
		{preset: "-1"},
		{preset: "custom", custom: "0", unit: "minutes"},
		{preset: "custom", custom: "1441", unit: "minutes"},
	} {
		if got, ok := normalizeCleanupDelay(tc.preset, tc.custom, tc.unit); ok {
			t.Fatalf("normalizeCleanupDelay(%q,%q,%q) = %d,true; want invalid", tc.preset, tc.custom, tc.unit, got)
		}
	}
}

func TestOutboxErrorHint(t *testing.T) {
	cases := map[string]string{
		"telegram sendMessage: 429 Too Many Requests retry_after=4": "Telegram 限流 429",
		"telegram sendMessage: 502 bad gateway":                     "Telegram 5xx",
		"context deadline exceeded":                                 "网络超时",
		"notification queue is full":                                "本地通知队列已满",
		"":                                                          "无错误",
	}
	for raw, want := range cases {
		if got := outboxErrorHint(raw); !strings.Contains(got, want) {
			t.Fatalf("outboxErrorHint(%q) = %q, want contains %q", raw, got, want)
		}
	}
}

func TestAdminTemplateRendersSearchableTallSavedGroups(t *testing.T) {
	var buf bytes.Buffer
	err := adminTemplate.Execute(&buf, pageData{
		Version:         "2.3.0",
		AdminRoleLabel:  "宿主",
		CanManageGlobal: true,
		Groups: []storage.Group{{
			ChatID:    -1003720457420,
			Title:     "测试群",
			UpdatedAt: time.Date(2026, 7, 6, 14, 24, 0, 0, time.UTC),
		}},
		ChatNames: map[int64]string{-1003720457420: "测试群"},
	})
	if err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	wants := []string{
		`data-admin-tab-target="groups"`,
		`id="saved-group-search"`,
		`class="scroll tall"`,
		`id="saved-group-rows"`,
		`data-search="测试群 -1003720457420"`,
		`querySelectorAll('#saved-group-rows tr[data-search]')`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Fatalf("admin template missing %q", want)
		}
	}
}

func TestAdminTemplateRendersReadableBroadcastManagement(t *testing.T) {
	var buf bytes.Buffer
	data := pageData{
		Version:                       "2.3.0",
		AdminRoleLabel:                "宿主",
		CanManageGlobal:               true,
		CanManageOperators:            true,
		CanManageBroadcastPermissions: true,
		Groups: []storage.Group{
			{ChatID: -1001, Title: "出款群", UpdatedAt: time.Date(2026, 7, 6, 14, 24, 0, 0, time.UTC)},
			{ChatID: -1002, Title: "扫码群引导", UpdatedAt: time.Date(2026, 7, 6, 14, 24, 0, 0, time.UTC)},
		},
		BGroups:              []storage.BroadcastGroup{{Name: "出款", ChatIDs: []int64{-1001}, ChatNames: []string{"出款群"}}},
		BroadcastMemberships: []storage.BroadcastGroup{{Name: "出款", ChatIDs: []int64{-1001}, ChatNames: []string{"出款群"}}},
		BOperators: []storage.GlobalOperator{{
			UserID:                              7611260151,
			Level:                               "primary",
			Status:                              "active",
			Remark:                              "柚子",
			PrivateCleanupEnabled:               true,
			PrivateCleanupTime:                  "08:30",
			PrivateCleanupBotDeleteAfterSeconds: 300,
			PrivateCleanupIncomingEnabled:       true,
			PrivateCleanupIncomingAfterSeconds:  45,
			CreatedAt:                           time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC),
		}, {
			UserID:    8453656635,
			Level:     "secondary",
			Status:    "active",
			CreatedBy: 7611260151,
			CreatedAt: time.Date(2026, 7, 6, 15, 30, 0, 0, time.UTC),
		}},
		Permissions: []storage.BroadcastPermission{{
			UserID:    7611260151,
			Target:    "group",
			GroupName: "出款",
			GrantedBy: 0,
		}, {
			UserID:    7611260151,
			Target:    "chat",
			ChatID:    -1002,
			GrantedBy: 0,
		}},
		PermissionFilterData: []storage.BroadcastPermission{{
			UserID: 7611260151, Target: "group", GroupName: "出款",
		}, {
			UserID: 7611260151, Target: "chat", ChatID: -1002,
		}},
		ChatNames: map[int64]string{-1001: "出款群", -1002: "扫码群引导"},
		WatchTargets: []storage.WatchTarget{{
			OwnerUserID:     7611260151,
			Address:         "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ",
			Label:           "收款地址",
			WatchIncome:     true,
			WatchExpense:    true,
			NotifyTRX:       false,
			MinNotifyAmount: "10",
			LatestTimestamp: time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC).UnixMilli(),
		}},
		OpLabels: map[int64]string{7611260151: "柚子"},
	}
	if err := adminTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	wants := []string{
		`data-admin-tab-target="broadcast"`,
		`data-admin-tab-target="permissions"`,
		`data-admin-tab-target="watch"`,
		`data-admin-tab-target="replace"`,
		`一级 / 下级操作人`,
		`一级操作人`,
		`下级操作人`,
		`class="toolbar-forms"`,
		`添加群组到分组`,
		`从分组移除群组`,
		`class="permission-panels"`,
		`授权广播目标`,
		`取消广播权限`,
		`私聊清空`,
		`action="/admin/operator/cleanup"`,
		`class="cleanup-form"`,
		`name="bot_delete_after"`,
		`name="incoming_enabled"`,
		`name="incoming_delete_after"`,
		`bot提示 5分钟后`,
		`用户临时消息 45秒后`,
		`只处理该操作人与机器人私聊`,
		`value="08:30"`,
		`data-mode="add"`,
		`data-mode="remove"`,
		`class="member-group-select"`,
		`class="member-chat-select"`,
		`data-groups="[&#34;出款&#34;]"`,
		`mode==='remove'?inGroup:!inGroup`,
		`data-mode="grant"`,
		`data-mode="revoke"`,
		`class="permission-user-select"`,
		`class="permission-group-select"`,
		`class="permission-chat-select"`,
		`data-users="[&#34;7611260151&#34;]"`,
		`mode==='revoke'?hasPermission:!hasPermission`,
		`地址监听`,
		`class="watch-panel"`,
		`TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ`,
		`收款地址`,
		`柚子`,
		`未备注操作人 ****6635`,
		`2026-07-06 15:30`,
		`后台管理`,
		`document.querySelectorAll('.permission-form')`,
		`localStorage.setItem('ledger-admin-tab',name)`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Fatalf("admin template missing %q", want)
		}
	}
	if strings.Contains(html, ">0</td>") {
		t.Fatal("admin permission table should not render raw granted_by=0")
	}
	if strings.Contains(html, "柚子（7611260151）") {
		t.Fatal("admin template should not directly render full UID beside operator name")
	}
	visible := renderedText(html)
	for _, fullUID := range []string{"7611260151", "8453656635"} {
		if strings.Contains(visible, fullUID) {
			t.Fatalf("admin template should not render full UID in visible text: %s", fullUID)
		}
	}
	for _, masked := range []string{"未备注操作人 ****6635", "柚子"} {
		if !strings.Contains(visible, masked) {
			t.Fatalf("admin template visible text missing operator label %q", masked)
		}
	}
	if !strings.Contains(adminHTML, ".permission-panel form{display:grid;grid-template-columns:repeat(2,minmax(0,1fr))") {
		t.Fatal("permission form should wrap inside each panel instead of using a wide five-column row")
	}
	if !strings.Contains(adminHTML, ".tab-card{display:none}.tab-card.active{display:block}") {
		t.Fatal("admin modules should be split into tabs")
	}
	if strings.Contains(adminHTML, "grid-template-columns:minmax(180px,.8fr) 150px") {
		t.Fatal("permission form should not use the old overflowing five-column layout")
	}
	if strings.Contains(html, `data-admin-tab-target="outbox"`) || strings.Contains(html, `发送网关 / Outbox 状态`) {
		t.Fatal("admin main page should not render a permanent global outbox status tab")
	}
}

func TestAdminTemplateForOperatorOnlyRendersWatchTab(t *testing.T) {
	var buf bytes.Buffer
	data := pageData{
		Version:        "2.3.0",
		AdminRoleLabel: "操作人",
		WatchTargets: []storage.WatchTarget{{
			OwnerUserID:     7611260151,
			Address:         "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ",
			WatchIncome:     true,
			WatchExpense:    true,
			MinNotifyAmount: "0",
		}},
	}
	if err := adminTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	if !strings.Contains(html, `data-admin-tab-target="watch"`) || !strings.Contains(html, `TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ`) {
		t.Fatal("operator admin page should render address watch tab")
	}
	if !strings.Contains(html, `class="tab-btn active" type="button" data-admin-tab-target="watch"`) {
		t.Fatal("operator admin page should default to active watch tab")
	}
	if !strings.Contains(html, `class="card wide tab-card active" data-admin-tab="watch"`) {
		t.Fatal("operator admin page should show watch card without waiting for JavaScript")
	}
	for _, blocked := range []string{`data-admin-tab-target="groups"`, `data-admin-tab-target="broadcast"`, `data-admin-tab-target="permissions"`, `data-admin-tab-target="outbox"`, `data-admin-tab-target="replace"`, `发送网关 / Outbox 状态`, `广播权限`, `广播替换`} {
		if strings.Contains(html, blocked) {
			t.Fatalf("operator admin page should not render global module %q", blocked)
		}
	}
}

func TestAdminTemplateForPrimaryRendersOwnedGroupAndDelegatedPermissionTools(t *testing.T) {
	var buf bytes.Buffer
	data := pageData{
		Version:                       "2.4.2",
		AdminRoleLabel:                "一级操作人",
		AdminUserID:                   3003,
		CanManageOperators:            true,
		CanManageBroadcastPermissions: true,
		CanManageBroadcastGroups:      true,
		Groups: []storage.Group{
			{ChatID: -1001, Title: "owned chat"},
		},
		BGroups: []storage.BroadcastGroup{
			{Name: "owned", OwnerUserID: 3003, ChatIDs: []int64{-1001}, ChatNames: []string{"owned chat"}},
			{Name: "shared-use", OwnerUserID: 4004, ChatIDs: []int64{-1001}, ChatNames: []string{"owned chat"}},
		},
		BroadcastMemberships: []storage.BroadcastGroup{
			{Name: "owned", OwnerUserID: 3003, ChatIDs: []int64{-1001}, ChatNames: []string{"owned chat"}},
			{Name: "shared-use", OwnerUserID: 4004, ChatIDs: []int64{-1001}, ChatNames: []string{"owned chat"}},
		},
		BOperators: []storage.GlobalOperator{
			{UserID: 3004, Level: "secondary", Status: "active", ParentUserID: 3003, Remark: "own active secondary"},
			{UserID: 3005, Level: "secondary", Status: "disabled", ParentUserID: 3003, Remark: "own disabled secondary"},
		},
	}
	if err := adminTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{`data-admin-tab-target="broadcast"`, `data-admin-tab-target="permissions"`, `action="/admin/group/save"`, `action="/admin/group/rename"`, `action="/admin/operator/save"`, `action="/admin/permission/grant"`, `data-admin-tab-target="watch"`, `可管理`, `仅可使用`} {
		if !strings.Contains(html, want) {
			t.Fatalf("primary admin page missing %q", want)
		}
	}
	for _, blocked := range []string{`data-admin-tab-target="replace"`} {
		if strings.Contains(html, blocked) {
			t.Fatalf("primary admin page exposed host module %q", blocked)
		}
	}
	for _, want := range []string{`name="level" value="secondary"`, `>禁用</button>`, `>启用</button>`} {
		if !strings.Contains(html, want) {
			t.Fatalf("primary operator controls missing %q", want)
		}
	}
	if strings.Contains(html, `name="level" value="primary"`) || strings.Contains(html, `<select name="level">`) {
		t.Fatal("primary page should not offer primary creation or a mutable level selector")
	}
}

func TestAdminTemplateForHostManagesPrimaryAndLabelsDisabledAction(t *testing.T) {
	var buf bytes.Buffer
	data := pageData{
		Version:            "2.4.3",
		AdminUserID:        1001,
		AdminRoleLabel:     "宿主",
		CanManageGlobal:    true,
		CanManageOperators: true,
		PermissionOperators: []storage.GlobalOperator{
			{UserID: 3003, Level: "primary", Status: "active", Remark: "active primary"},
		},
		PrimaryOperators: []storage.GlobalOperator{
			{UserID: 3003, Level: "primary", Status: "active", Remark: "active primary"},
		},
		BOperators: []storage.GlobalOperator{
			{UserID: 3003, Level: "primary", Status: "active", Remark: "active primary"},
			{UserID: 3004, Level: "primary", Status: "disabled", Remark: "disabled primary"},
			{UserID: 3005, Level: "secondary", Status: "active", ParentUserID: 3003, Remark: "managed by primary"},
			{UserID: 1001, Level: "host", Status: "active", Remark: "environment owner"},
		},
	}
	if err := adminTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	for _, want := range []string{`<select name="level"><option value="primary">一级操作人</option><option value="secondary">下级操作人</option></select>`, `name="parent_user_id"`, `>禁用</button>`, `>启用</button>`, `环境配置`} {
		if !strings.Contains(html, want) {
			t.Fatalf("host operator controls missing %q", want)
		}
	}
	if strings.Contains(html, `name="level" value="secondary"><span class="field-label">新增自己的下级操作人`) {
		t.Fatal("host page should use the explicit level and primary-parent selectors")
	}
}

func TestOutboxStatusRejectsOperator(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/outbox/status", nil)
	req = req.WithContext(context.WithValue(req.Context(), adminContextKey{}, adminauth.Session{UserID: 3003, Role: adminauth.RoleOperator}))
	s.outboxStatus(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestAdminListPaginationAndSearch(t *testing.T) {
	groups := make([]storage.Group, 0, 125)
	for i := 0; i < 125; i++ {
		groups = append(groups, storage.Group{ChatID: int64(-100000 - i), Title: fmt.Sprintf("结算群 %03d", i)})
	}
	page, pager := pageAdminGroups(groups, adminListFilters{GroupPage: 2})
	if len(page) != adminListPageSize || pager.ItemFrom != 51 || pager.ItemTo != 100 || !pager.HasPrev || !pager.HasNext {
		t.Fatalf("page 2 = len %d pager %+v", len(page), pager)
	}
	page, pager = pageAdminGroups(groups, adminListFilters{GroupQuery: "结算群 124", GroupPage: 99})
	if len(page) != 1 || page[0].Title != "结算群 124" || pager.Page != 1 || pager.Total != 1 {
		t.Fatalf("searched page = %+v pager %+v", page, pager)
	}
}

func TestAdminListURLPreservesIndependentFilters(t *testing.T) {
	filters := adminListFilters{GroupQuery: "财务", GroupPage: 2, BroadcastQuery: "夜班", BroadcastPage: 3}
	got := adminListURL(filters, "groups", 3)
	for _, want := range []string{"groups_q=%E8%B4%A2%E5%8A%A1", "groups_page=3", "broadcast_q=%E5%A4%9C%E7%8F%AD", "broadcast_page=3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("pagination URL %q missing %q", got, want)
		}
	}
}

func renderedText(html string) string {
	var b strings.Builder
	inTag := false
	skipUntil := ""
	lower := strings.ToLower(html)
	for i := 0; i < len(html); i++ {
		if skipUntil != "" {
			if strings.HasPrefix(lower[i:], skipUntil) {
				i += len(skipUntil) - 1
				skipUntil = ""
				inTag = false
			}
			continue
		}
		if inTag {
			if html[i] == '>' {
				inTag = false
			}
			continue
		}
		if html[i] == '<' {
			if strings.HasPrefix(lower[i:], "<script") {
				skipUntil = "</script>"
			} else if strings.HasPrefix(lower[i:], "<style") {
				skipUntil = "</style>"
			}
			inTag = true
			continue
		}
		b.WriteByte(html[i])
	}
	return b.String()
}
