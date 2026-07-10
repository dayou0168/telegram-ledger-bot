package adminweb

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
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

func TestAdminTemplateRendersSearchableTallSavedGroups(t *testing.T) {
	var buf bytes.Buffer
	err := adminTemplate.Execute(&buf, pageData{
		Version: "2.2.0",
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
		Version: "2.2.0",
		Groups: []storage.Group{
			{ChatID: -1001, Title: "出款群", UpdatedAt: time.Date(2026, 7, 6, 14, 24, 0, 0, time.UTC)},
			{ChatID: -1002, Title: "扫码群引导", UpdatedAt: time.Date(2026, 7, 6, 14, 24, 0, 0, time.UTC)},
		},
		BGroups: []storage.BroadcastGroup{{Name: "出款", ChatIDs: []int64{-1001}, ChatNames: []string{"出款群"}}},
		BOperators: []storage.BroadcastOperator{{
			UserID: 7611260151,
			Status: "active",
			Remark: "柚子",
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
		ChatNames: map[int64]string{-1001: "出款群", -1002: "扫码群引导"},
		OpLabels:  map[int64]string{7611260151: "柚子（7611260151）"},
	}
	if err := adminTemplate.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	html := buf.String()
	wants := []string{
		`class="toolbar-forms"`,
		`添加群组到分组`,
		`从分组移除群组`,
		`class="permission-panels"`,
		`授权广播目标`,
		`取消广播权限`,
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
		`柚子（7611260151）`,
		`后台管理`,
		`document.querySelectorAll('.permission-form')`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Fatalf("admin template missing %q", want)
		}
	}
	if strings.Contains(html, ">0</td>") {
		t.Fatal("admin permission table should not render raw granted_by=0")
	}
	if !strings.Contains(adminHTML, ".permission-panel form{display:grid;grid-template-columns:repeat(2,minmax(0,1fr))") {
		t.Fatal("permission form should wrap inside each panel instead of using a wide five-column row")
	}
	if strings.Contains(adminHTML, "grid-template-columns:minmax(180px,.8fr) 150px") {
		t.Fatal("permission form should not use the old overflowing five-column layout")
	}
}
