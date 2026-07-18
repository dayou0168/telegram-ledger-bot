package bot

import "testing"

func TestParseLedgerDepositWithRate(t *testing.T) {
	cmd, ok := parseLedger("+100/7 备注")
	if !ok {
		t.Fatal("expected deposit command")
	}
	if cmd.Kind != "deposit" {
		t.Fatalf("kind = %s", cmd.Kind)
	}
	if formatRat(cmd.Amount, 2) != "100" {
		t.Fatalf("amount = %s", formatRat(cmd.Amount, 2))
	}
	if formatRat(cmd.Rate, 2) != "7" {
		t.Fatalf("rate = %s", formatRat(cmd.Rate, 2))
	}
	if cmd.Remark != "备注" {
		t.Fatalf("remark = %s", cmd.Remark)
	}
}

func TestParseLedgerDepositWithMultiplierAndRate(t *testing.T) {
	cmd, ok := parseLedger("+10000*5/7.1")
	if !ok {
		t.Fatal("expected deposit command")
	}
	if formatRat(cmd.Multiplier, 2) != "5" {
		t.Fatalf("multiplier = %s", formatRat(cmd.Multiplier, 2))
	}
	if formatRat(cmd.Rate, 2) != "7.1" {
		t.Fatalf("rate = %s", formatRat(cmd.Rate, 2))
	}
}

func TestParseLedgerDepositWithInlineFee(t *testing.T) {
	cmd, ok := parseLedger("+1000*12%")
	if !ok {
		t.Fatal("expected deposit command")
	}
	if formatRat(cmd.FeeRate, 2) != "12" {
		t.Fatalf("fee = %s", formatRat(cmd.FeeRate, 2))
	}
	if formatRat(cmd.Multiplier, 2) != "1" {
		t.Fatalf("multiplier = %s", formatRat(cmd.Multiplier, 2))
	}
}

func TestParseLedgerPayoutRequiresUSDT(t *testing.T) {
	if _, ok := parseLedger("下发100"); ok {
		t.Fatal("plain CNY payout should be disabled")
	}
	cmd, ok := parseLedger("下发100U")
	if !ok {
		t.Fatal("expected USDT payout")
	}
	if cmd.Kind != "payout" || !cmd.IsUSDT {
		t.Fatalf("unexpected payout command: %+v", cmd)
	}
}

func TestOpenBillCommandOnlyPlusZero(t *testing.T) {
	if !isBillCommand("+0") || !isOpenBillCommand("+0") {
		t.Fatal("+0 should be an open bill query command")
	}
	if got := classifyMessageCommand("+0", "supergroup"); got != "ledger_bill" {
		t.Fatalf("+0 command classification = %q, want ledger_bill", got)
	}
	for _, text := range []string{"账单", "显示账单", "+100", "下发100U"} {
		if isOpenBillCommand(text) {
			t.Fatalf("%q should not bypass ledger permission checks", text)
		}
	}
}

func TestParseLedgerPayoutWithMultiplierAndRate(t *testing.T) {
	cmd, ok := parseLedger("下发5000*5/7.1")
	if !ok {
		t.Fatal("expected payout command")
	}
	if cmd.Kind != "payout" || cmd.IsUSDT {
		t.Fatalf("unexpected payout command: %+v", cmd)
	}
	if formatRat(cmd.Multiplier, 2) != "5" {
		t.Fatalf("multiplier = %s", formatRat(cmd.Multiplier, 2))
	}
	if formatRat(cmd.Rate, 2) != "7.1" {
		t.Fatalf("rate = %s", formatRat(cmd.Rate, 2))
	}
}

func TestFormatAmountRoundsHalfUpToTwoDecimals(t *testing.T) {
	cases := map[string]string{
		"100":     "100",
		"100.554": "100.55",
		"100.555": "100.56",
		"100.556": "100.56",
		"100/6":   "16.67",
		"0.004":   "0",
		"0.005":   "0.01",
	}
	for raw, want := range cases {
		got := formatAmount(parseRat(raw))
		if got != want {
			t.Fatalf("formatAmount(%q) = %s, want %s", raw, got, want)
		}
	}
}

func TestParseSettings(t *testing.T) {
	fee, ok := parseSetting("设置费率3%")
	if !ok || fee.Kind != "fee" || formatRat(fee.Value, 2) != "3" {
		t.Fatalf("unexpected fee setting: %+v ok=%v", fee, ok)
	}
	rate, ok := parseSetting("设置汇率 7.53")
	if !ok || rate.Kind != "exchange_rate" || formatRat(rate.Value, 2) != "7.53" {
		t.Fatalf("unexpected rate setting: %+v ok=%v", rate, ok)
	}
	cutoff, ok := parseSetting("设置日切04")
	if !ok || cutoff.Kind != "cutoff" || cutoff.CutoffHour != 4 {
		t.Fatalf("unexpected cutoff setting: %+v ok=%v", cutoff, ok)
	}
	for _, text := range []string{"关闭日切", "设置日切-1", "设置日切 -1"} {
		disabled, ok := parseSetting(text)
		if !ok || disabled.Kind != "cutoff" || disabled.CutoffHour != cutoffDisabledHour {
			t.Fatalf("parseSetting(%q) = %+v, %v; want disabled cutoff", text, disabled, ok)
		}
	}
	for _, text := range []string{"设置日切-2", "设置日切24"} {
		if cmd, ok := parseSetting(text); ok {
			t.Fatalf("parseSetting(%q) = %+v, true; want invalid", text, cmd)
		}
	}
}

func TestParseMentions(t *testing.T) {
	got := parseMentions("添加操作员 @Alice_01 @alice_01 @Bob002")
	if len(got) != 2 || got[0] != "alice_01" || got[1] != "bob002" {
		t.Fatalf("mentions = %#v", got)
	}
}

func TestParseZRateSetting(t *testing.T) {
	cmd, ok := parseZRateSetting("设置汇率 Z3 +0.5")
	if !ok || cmd.Rank != 3 || formatSigned(cmd.Offset) != "+0.5" {
		t.Fatalf("unexpected z setting: %+v ok=%v", cmd, ok)
	}
	cmd, ok = parseZRateSetting("Z1 -0")
	if !ok || cmd.Rank != 1 || formatSigned(cmd.Offset) != "0" {
		t.Fatalf("unexpected z zero setting: %+v ok=%v", cmd, ok)
	}
	if !isZ0Command("z0") {
		t.Fatalf("z0 should be recognized")
	}
}

func TestParseTRXAddressQuery(t *testing.T) {
	address := "TGhAAySHUUcEGua33pZZ88wP3bA6X5eQuZ"
	for _, text := range []string{
		"查询" + address,
		"查询TRX地址 " + address,
		"trx " + address,
	} {
		got, ok := parseTRXAddressQuery(text)
		if !ok || got != address {
			t.Fatalf("parseTRXAddressQuery(%q) = %q %v", text, got, ok)
		}
	}
	if _, ok := parseTRXAddressQuery(address); ok {
		t.Fatal("bare address should stay in address validation flow")
	}
}

func TestParseClearScope(t *testing.T) {
	for _, command := range []string{"清除当前账期", "清除今日账单", "删除账单", "清除账单"} {
		if scope, ok := parseClearScope(command); !ok || scope != "current" {
			t.Fatalf("current period clear parse for %q = %q %v", command, scope, ok)
		}
	}
	if _, ok := parseClearScope("删除全部账单"); ok {
		t.Fatal("all-record deletion command must stay closed")
	}
	if _, ok := parseClearScope("删除昨天账单"); ok {
		t.Fatal("unexpected clear scope")
	}
}
