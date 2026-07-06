package bot

import (
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

type commandKind string

const (
	commandNone     commandKind = ""
	commandStart    commandKind = "start"
	commandStop     commandKind = "stop"
	commandAddWatch commandKind = "add_watch"
	commandDelWatch commandKind = "del_watch"
)

type ledgerCommand struct {
	Kind   string
	Amount *big.Rat
	Rate   *big.Rat
	IsUSDT bool
	Remark string
}

type settingCommand struct {
	Kind       string
	Value      *big.Rat
	CutoffHour int
}

type zRateCommand struct {
	Rank   int
	Offset *big.Rat
}

var (
	depositPattern  = regexp.MustCompile(`^\+([0-9]+(?:\.[0-9]+)?)(?:/([0-9]+(?:\.[0-9]+)?))?([uU])?(?:\s+(.+))?$`)
	payoutPattern   = regexp.MustCompile(`^下发\s*([0-9]+(?:\.[0-9]+)?)([uU])(?:\s+(.+))?$`)
	feePattern      = regexp.MustCompile(`^设置(?:入款|下发)?费率\s*([+-]?[0-9]+(?:\.[0-9]+)?)%?$`)
	ratePattern     = regexp.MustCompile(`^设置(?:入款|下发)?汇率\s*([+-]?[0-9]+(?:\.[0-9]+)?)$`)
	cutoffPattern   = regexp.MustCompile(`^设置日切\s*(-?[0-9]{1,2})$`)
	zRatePattern    = regexp.MustCompile(`^(?:设置汇率\s*)?[zZ]([1-9]|10)(?:\s*([+-]\s*[0-9]+(?:\.[0-9]+)?))?$`)
	watchAddPattern = regexp.MustCompile(`^(?:添加监听地址|设置监听地址|添加地址|设置地址|监听)\s+(\S+)(?:\s+(.+))?$`)
	watchDelPattern = regexp.MustCompile(`^(?:删除监听地址|删除地址|取消监听)\s+(\S+)$`)
	mentionPattern  = regexp.MustCompile(`@([A-Za-z0-9_]{5,32})`)
	trc20Pattern    = regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
)

func parseLedger(text string) (ledgerCommand, bool) {
	text = strings.TrimSpace(text)
	if match := depositPattern.FindStringSubmatch(text); match != nil {
		amount := parseRat(match[1])
		rate := parseRat(match[2])
		return ledgerCommand{
			Kind:   "deposit",
			Amount: amount,
			Rate:   rate,
			IsUSDT: strings.EqualFold(match[3], "u"),
			Remark: strings.TrimSpace(match[4]),
		}, amount != nil
	}
	if match := payoutPattern.FindStringSubmatch(text); match != nil {
		amount := parseRat(match[1])
		return ledgerCommand{
			Kind:   "payout",
			Amount: amount,
			IsUSDT: true,
			Remark: strings.TrimSpace(match[3]),
		}, amount != nil
	}
	return ledgerCommand{}, false
}

func parseSetting(text string) (settingCommand, bool) {
	text = strings.TrimSpace(text)
	if match := feePattern.FindStringSubmatch(text); match != nil {
		value := parseRat(match[1])
		return settingCommand{Kind: "fee", Value: value}, value != nil
	}
	if match := ratePattern.FindStringSubmatch(text); match != nil {
		value := parseRat(match[1])
		return settingCommand{Kind: "exchange_rate", Value: value}, value != nil
	}
	if match := cutoffPattern.FindStringSubmatch(text); match != nil {
		hour, err := strconv.Atoi(match[1])
		if err != nil {
			return settingCommand{}, false
		}
		return settingCommand{Kind: "cutoff", CutoffHour: hour}, hour >= 0 && hour <= 23
	}
	if text == "关闭日切" {
		return settingCommand{Kind: "cutoff", CutoffHour: 0}, true
	}
	return settingCommand{}, false
}

func isBillCommand(text string) bool {
	switch strings.TrimSpace(text) {
	case "+0", "显示账单", "账单":
		return true
	default:
		return false
	}
}

func isZ0Command(text string) bool {
	return strings.EqualFold(strings.TrimSpace(text), "Z0")
}

func parseZRateSetting(text string) (zRateCommand, bool) {
	match := zRatePattern.FindStringSubmatch(strings.TrimSpace(text))
	if match == nil {
		return zRateCommand{}, false
	}
	rank, err := strconv.Atoi(match[1])
	if err != nil || rank < 1 || rank > 10 {
		return zRateCommand{}, false
	}
	offset := big.NewRat(0, 1)
	if raw := strings.ReplaceAll(match[2], " ", ""); raw != "" {
		offset = parseRat(raw)
		if offset == nil {
			return zRateCommand{}, false
		}
	}
	return zRateCommand{Rank: rank, Offset: offset}, true
}

func parseUndoKind(text string) (string, bool) {
	switch strings.TrimSpace(text) {
	case "撤销":
		return "", true
	case "撤销入款":
		return "deposit", true
	case "撤销下发":
		return "payout", true
	default:
		return "", false
	}
}

func parseClearScope(text string) (string, bool) {
	switch strings.TrimSpace(text) {
	case "清除今日账单", "删除账单", "清除账单":
		return "today", true
	case "清除全部账单", "删除全部账单":
		return "all", true
	default:
		return "", false
	}
}

func isOperatorWriteCommand(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "添加操作员") ||
		strings.HasPrefix(text, "删除操作员") ||
		strings.HasPrefix(text, "设置操作人") ||
		strings.HasPrefix(text, "移除操作人") ||
		strings.HasPrefix(text, "删除操作人")
}

func isOperatorListCommand(text string) bool {
	switch strings.TrimSpace(text) {
	case "管理员", "权限人", "显示操作员":
		return true
	default:
		return false
	}
}

func parseMentions(text string) []string {
	matches := mentionPattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]struct{}, len(matches))
	var usernames []string
	for _, match := range matches {
		username := NormalizeMention(match[1])
		if username == "" {
			continue
		}
		if _, ok := seen[username]; ok {
			continue
		}
		seen[username] = struct{}{}
		usernames = append(usernames, username)
	}
	return usernames
}

func parseRat(raw string) *big.Rat {
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

func NormalizeMention(username string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
}

func formatRat(value *big.Rat, precision int) string {
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

func formatAmount(value *big.Rat) string {
	if value == nil {
		return "0.00"
	}
	text := value.FloatString(2)
	if text == "-0.00" {
		return "0.00"
	}
	return text
}

func isTRC20Address(text string) bool {
	return trc20Pattern.MatchString(strings.TrimSpace(text))
}

func parseTRXAddressQuery(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	prefixes := []string{"查询地址", "查询TRX地址", "查询trx地址", "查询TRX", "查询trx", "查询"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(text, prefix) {
			address := strings.TrimSpace(strings.TrimPrefix(text, prefix))
			if isTRC20Address(address) {
				return address, true
			}
		}
	}
	lower := strings.ToLower(text)
	for _, prefix := range []string{"trx ", "trx:", "地址 "} {
		if strings.HasPrefix(lower, prefix) {
			address := strings.TrimSpace(text[len(prefix):])
			if isTRC20Address(address) {
				return address, true
			}
		}
	}
	return "", false
}
