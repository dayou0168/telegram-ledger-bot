package adminweb

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/xuri/excelize/v2"
)

const adminCookieName = "ledger_admin_token"

type Server struct {
	cfg   config.Config
	store *storage.Store
}

type pageData struct {
	Version     string
	TokenUnset  bool
	Message     string
	Groups      []storage.Group
	BGroups     []storage.BroadcastGroup
	BOperators  []storage.BroadcastOperator
	Permissions []storage.BroadcastPermission
	Replace     storage.BroadcastReplaceSetting
	ChatNames   map[int64]string
}

type billData struct {
	Group        storage.Group
	DayKey       string
	Records      []storage.Record
	Summary      billSummary
	BillDays     []string
	PrevDay      string
	NextDay      string
	PrevPath     string
	NextPath     string
	FilterSuffix string
	DownloadPath string
	Query        string
	Field        string
}

type billSummary struct {
	Deposits         []storage.Record
	Payouts          []storage.Record
	DepositCount     int
	PayoutCount      int
	TotalDepositCNY  string
	TotalDepositUSDT string
	TotalPayoutUSDT  string
	BalanceUSDT      string
	ExchangeRate     string
	FeeRate          string
}

func New(cfg config.Config, store *storage.Store) *Server {
	return &Server{cfg: cfg, store: store}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.health)
	mux.HandleFunc("/b/", s.bill)
	mux.HandleFunc("/admin/login", s.login)
	mux.HandleFunc("/admin/logout", s.logout)
	mux.HandleFunc("/admin", s.withAuth(s.index))
	mux.HandleFunc("/admin/group/save", s.withAuth(s.saveGroup))
	mux.HandleFunc("/admin/group/delete", s.withAuth(s.deleteGroup))
	mux.HandleFunc("/admin/group/add", s.withAuth(s.addGroupChats))
	mux.HandleFunc("/admin/group/remove", s.withAuth(s.removeGroupChats))
	mux.HandleFunc("/admin/operator/save", s.withAuth(s.saveOperator))
	mux.HandleFunc("/admin/operator/disable", s.withAuth(s.disableOperator))
	mux.HandleFunc("/admin/permission/grant", s.withAuth(s.grantPermission))
	mux.HandleFunc("/admin/permission/revoke", s.withAuth(s.revokePermission))
	mux.HandleFunc("/admin/replace/save", s.withAuth(s.saveReplace))

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
	records, err := s.store.ListRecordsForDay(r.Context(), chatID, dayKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	field := normalizedBillField(r.URL.Query().Get("field"))
	records = filterBillRecords(records, query, field)
	if action == "download" {
		s.downloadBill(w, group, dayKey, records)
		return
	}
	days, err := s.store.ListBillDays(r.Context(), chatID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := billTemplate.Execute(w, billData{
		Group:        group,
		DayKey:       dayKey,
		Records:      records,
		Summary:      summarizeBill(group, records),
		BillDays:     days,
		PrevDay:      addDay(dayKey, -1),
		NextDay:      addDay(dayKey, 1),
		PrevPath:     billPath(chatID, addDay(dayKey, -1)) + billFilterSuffix(field, query),
		NextPath:     billPath(chatID, addDay(dayKey, 1)) + billFilterSuffix(field, query),
		FilterSuffix: billFilterSuffix(field, query),
		DownloadPath: billDownloadPath(chatID, dayKey, field, query),
		Query:        query,
		Field:        field,
	}); err != nil {
		log.Printf("render bill: %v", err)
	}
}

func (s *Server) downloadBill(w http.ResponseWriter, group storage.Group, dayKey string, records []storage.Record) {
	data, err := buildBillXLSX(group, dayKey, records)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	fileName := fmt.Sprintf("账单_%s_%s.xlsx", dayKey, safeFileName(group.Title, "ledger"))
	fallback := fmt.Sprintf("ledger_%s.xlsx", strings.ReplaceAll(dayKey, "-", ""))
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s", fallback, url.PathEscape(fileName)))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		renderLogin(w, s.cfg.AdminWebToken == "", "")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.cfg.AdminWebToken != "" && r.FormValue("password") != s.cfg.AdminWebToken {
		renderLogin(w, false, "密码不正确")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     adminCookieName,
		Value:    s.cfg.AdminWebToken,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400 * 7,
	})
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: adminCookieName, Path: "/admin", MaxAge: -1})
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminWebToken == "" {
			next(w, r)
			return
		}
		cookie, err := r.Cookie(adminCookieName)
		if err != nil || cookie.Value != s.cfg.AdminWebToken {
			http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	data, err := s.loadPageData(r.Context(), r.URL.Query().Get("msg"))
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
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
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
	if err := s.store.UpsertBroadcastGroup(r.Context(), name, 0, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectMsg(w, r, "分组已保存")
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ok, err := s.store.DeleteBroadcastGroup(r.Context(), r.FormValue("name"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		redirectMsg(w, r, "分组不存在")
		return
	}
	redirectMsg(w, r, "分组已删除")
}

func (s *Server) addGroupChats(w http.ResponseWriter, r *http.Request) {
	s.changeGroupChats(w, r, true)
}

func (s *Server) removeGroupChats(w http.ResponseWriter, r *http.Request) {
	s.changeGroupChats(w, r, false)
}

func (s *Server) changeGroupChats(w http.ResponseWriter, r *http.Request, add bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := r.FormValue("name")
	chatIDs := parseIDList(r.Form["chat_id"])
	var count int
	var err error
	if add {
		count, err = s.store.AddChatsToBroadcastGroup(r.Context(), name, chatIDs, time.Now())
	} else {
		count, err = s.store.RemoveChatsFromBroadcastGroup(r.Context(), name, chatIDs)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	action := "添加"
	if !add {
		action = "移除"
	}
	redirectMsg(w, r, fmt.Sprintf("已%s %d 个群", action, count))
}

func (s *Server) saveOperator(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("user_id")), 10, 64)
	if err != nil || userID <= 0 {
		redirectMsg(w, r, "操作人 UID 不正确")
		return
	}
	if err := s.store.UpsertBroadcastOperator(r.Context(), userID, 0, r.FormValue("remark"), time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectMsg(w, r, "操作人已保存")
}

func (s *Server) disableOperator(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	_, err := s.store.DisableBroadcastOperator(r.Context(), userID, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectMsg(w, r, "操作人已禁用")
}

func (s *Server) grantPermission(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	target := r.FormValue("target")
	chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	groupName := r.FormValue("group_name")
	if err := s.store.AddBroadcastPermission(r.Context(), userID, target, chatID, groupName, 0, time.Now()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectMsg(w, r, "权限已授权")
}

func (s *Server) revokePermission(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	userID, _ := strconv.ParseInt(r.FormValue("user_id"), 10, 64)
	target := r.FormValue("target")
	chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	groupName := r.FormValue("group_name")
	_, err := s.store.RemoveBroadcastPermission(r.Context(), userID, target, chatID, groupName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirectMsg(w, r, "权限已取消")
}

func (s *Server) saveReplace(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) loadPageData(ctx context.Context, message string) (pageData, error) {
	groups, err := s.store.ListGroups(ctx)
	if err != nil {
		return pageData{}, err
	}
	bgroups, err := s.store.ListBroadcastGroups(ctx)
	if err != nil {
		return pageData{}, err
	}
	operators, err := s.store.ListBroadcastOperators(ctx)
	if err != nil {
		return pageData{}, err
	}
	permissions, err := s.store.ListBroadcastPermissions(ctx)
	if err != nil {
		return pageData{}, err
	}
	replace, err := s.store.GetBroadcastReplaceSetting(ctx)
	if err != nil {
		return pageData{}, err
	}
	chatNames := make(map[int64]string, len(groups))
	for _, group := range groups {
		chatNames[group.ChatID] = group.Title
	}
	return pageData{
		Version:     config.Version,
		TokenUnset:  s.cfg.AdminWebToken == "",
		Message:     message,
		Groups:      groups,
		BGroups:     bgroups,
		BOperators:  operators,
		Permissions: permissions,
		Replace:     replace,
		ChatNames:   chatNames,
	}, nil
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

func renderLogin(w http.ResponseWriter, tokenUnset bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginTemplate.Execute(w, map[string]any{"TokenUnset": tokenUnset, "Message": message})
}

func chatLabel(group storage.Group) string {
	if group.Title != "" {
		return group.Title
	}
	return strconv.FormatInt(group.ChatID, 10)
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
	if len(day) == 8 {
		day = day[:4] + "-" + day[4:6] + "-" + day[6:]
	}
	if len(day) != 10 {
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
	return day.AddDate(0, 0, delta).Format("20060102")
}

func buildBillXLSX(group storage.Group, dayKey string, records []storage.Record) ([]byte, error) {
	file := excelize.NewFile()
	defer func() { _ = file.Close() }()
	sheet := "账单"
	file.SetSheetName("Sheet1", sheet)
	headers := []string{"类型", "时间", "金额", "汇率", "操作人", "备注"}
	for i, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		_ = file.SetCellValue(sheet, cell, header)
	}
	for i, record := range records {
		row := i + 2
		values := []any{
			billKind(record.Kind),
			record.CreatedAt.Format("2006-01-02 15:04:05"),
			billAmount(record),
			billNumber(record.Rate, 8),
			record.ActorName,
			record.Remark,
		}
		for col, value := range values {
			cell, _ := excelize.CoordinatesToCellName(col+1, row)
			_ = file.SetCellValue(sheet, cell, value)
		}
	}
	title := fmt.Sprintf("%s %s", group.Title, dayKey)
	_ = file.SetCellValue(sheet, "H1", title)
	_ = file.SetColWidth(sheet, "A", "A", 12)
	_ = file.SetColWidth(sheet, "B", "B", 22)
	_ = file.SetColWidth(sheet, "C", "C", 26)
	_ = file.SetColWidth(sheet, "D", "D", 12)
	_ = file.SetColWidth(sheet, "E", "E", 24)
	_ = file.SetColWidth(sheet, "F", "F", 36)
	style, _ := file.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Color: "0E1B2F"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"F4F7FB"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
	})
	_ = file.SetCellStyle(sheet, "A1", "F1", style)
	var buf bytes.Buffer
	if err := file.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
	return amount + "/" + rate + "=" + result + "U"
}

func billKind(kind string) string {
	if kind == "payout" {
		return "下发"
	}
	return "入款"
}

func summarizeBill(group storage.Group, records []storage.Record) billSummary {
	summary := billSummary{
		ExchangeRate: billExchangeRateDisplay(group),
		FeeRate:      group.FeeRate,
	}
	totalDepositCNY := big.NewRat(0, 1)
	totalDepositUSDT := big.NewRat(0, 1)
	totalPayoutUSDT := big.NewRat(0, 1)
	for _, record := range records {
		result := parseBillRat(record.ResultUSDT)
		if result == nil {
			result = big.NewRat(0, 1)
		}
		switch record.Kind {
		case "deposit":
			summary.Deposits = append(summary.Deposits, record)
			totalDepositUSDT.Add(totalDepositUSDT, result)
			if strings.EqualFold(record.Currency, "CNY") {
				if amount := parseBillRat(record.Amount); amount != nil {
					totalDepositCNY.Add(totalDepositCNY, amount)
				}
			}
		case "payout":
			summary.Payouts = append(summary.Payouts, record)
			totalPayoutUSDT.Add(totalPayoutUSDT, result)
		}
	}
	balance := new(big.Rat).Sub(totalDepositUSDT, totalPayoutUSDT)
	summary.DepositCount = len(summary.Deposits)
	summary.PayoutCount = len(summary.Payouts)
	summary.TotalDepositCNY = formatBillRat(totalDepositCNY, 2)
	summary.TotalDepositUSDT = formatBillRat(totalDepositUSDT, 2)
	summary.TotalPayoutUSDT = formatBillRat(totalPayoutUSDT, 2)
	summary.BalanceUSDT = formatBillRat(balance, 2)
	if summary.FeeRate == "" {
		summary.FeeRate = "0"
	}
	summary.FeeRate = billNumber(summary.FeeRate, 2)
	return summary
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

func normalizedBillField(field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "actor", "remark", "amount":
		return strings.ToLower(strings.TrimSpace(field))
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

var adminTemplate = template.Must(template.New("admin").Funcs(template.FuncMap{
	"chatLabel":        chatLabel,
	"permissionTarget": permissionTarget,
}).Parse(adminHTML))

var loginTemplate = template.Must(template.New("login").Parse(loginHTML))

var billTemplate = template.Must(template.New("bill").Funcs(template.FuncMap{
	"billAmount": billAmount,
	"billKind":   billKind,
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
{{if .TokenUnset}}<div class="warn">当前没有配置 ADMIN_WEB_TOKEN，测试环境可直接进入；公网部署请务必设置。</div>{{end}}
{{if .Message}}<div class="err">{{.Message}}</div>{{end}}
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
:root{--bg:#eaf1f7;--panel:#fff;--line:#c8d6e6;--ink:#0e1b2f;--muted:#5b6f88;--navy:#14223a;--gold:#d8b45d;--blue:#2d6cdf}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font-family:Arial,"Microsoft YaHei",sans-serif;font-size:14px}
.wrap{max-width:1240px;margin:0 auto;padding:22px}
.top{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:18px 20px;display:flex;justify-content:space-between;gap:16px;align-items:center}
.brand{color:#b97914;font-weight:700;margin-bottom:5px}.title{font-size:28px;font-weight:800}.sub{color:var(--muted)}
.btn{height:38px;border:0;border-radius:6px;background:var(--navy);color:#fff;font-weight:700;padding:0 16px;cursor:pointer;text-decoration:none;display:inline-flex;align-items:center;justify-content:center}
.btn.secondary{background:#fff;color:var(--navy);border:1px solid var(--line)}
.msg{margin-top:14px;background:#eef8ff;border:1px solid #b8d8ff;color:#16437b;border-radius:6px;padding:10px 12px}
.warn{margin-top:14px;background:#fff7dd;border:1px solid #e1bd5f;border-radius:6px;padding:10px 12px}
.grid{margin-top:16px;display:grid;grid-template-columns:1fr 1fr;gap:16px;align-items:start}
.card{background:var(--panel);border:1px solid var(--line);border-top:4px solid var(--gold);border-radius:8px;padding:18px;min-width:0}
.card.wide{grid-column:1 / -1}
h2{font-size:21px;margin:0 0 12px}.hint{color:var(--muted);margin:0 0 12px;line-height:1.55}
.row{display:grid;grid-template-columns:1fr 1fr auto;gap:8px;margin-bottom:8px}.row.two{grid-template-columns:1fr auto}.row.one{grid-template-columns:1fr}
input,select,textarea{border:1px solid #b8c8dc;border-radius:6px;background:#fff;color:var(--ink);min-height:38px;padding:8px 10px;font-size:14px;min-width:0}
select[multiple]{min-height:150px}.full{width:100%}
table{width:100%;border-collapse:collapse;margin-top:10px}th,td{border:1px solid #dce5ef;padding:10px;text-align:center;vertical-align:middle}th{background:#f4f7fb;font-weight:800}
.scroll{max-height:260px;overflow:auto;border:1px solid #dce5ef;border-radius:6px}.scroll table{margin:0;border:0}.scroll th:first-child,.scroll td:first-child{border-left:0}.scroll th:last-child,.scroll td:last-child{border-right:0}
.pill{display:inline-block;border:1px solid #d5e1ec;background:#f7fafc;border-radius:999px;padding:3px 9px;color:#40566f}
.actions{display:flex;gap:8px;flex-wrap:wrap}.mini{height:32px;padding:0 10px}
@media(max-width:900px){.grid{grid-template-columns:1fr}.top{align-items:flex-start;flex-direction:column}.row,.row.two{grid-template-columns:1fr}.btn{width:100%}}
</style>
</head>
<body><main class="wrap">
<section class="top">
<div><div class="brand">Telegram 记账机器人</div><div class="title">后台管理</div><div class="sub">Go v{{.Version}} · 群组、广播分组、操作人和权限</div></div>
<div class="actions"><a class="btn secondary" href="/admin">刷新</a><a class="btn secondary" href="/admin/logout">退出</a></div>
</section>
{{if .TokenUnset}}<div class="warn">当前没有配置 ADMIN_WEB_TOKEN，公网部署请先设置后台密码。</div>{{end}}
{{if .Message}}<div class="msg">{{.Message}}</div>{{end}}

<section class="grid">
<div class="card">
<h2>已保存群组</h2>
<p class="hint">机器人被邀请进群，或群内有人发言后会自动保存群名；群改名后也会更新。</p>
<div class="scroll"><table><thead><tr><th>群名</th><th>群ID</th><th>更新时间</th></tr></thead><tbody>
{{range .Groups}}<tr><td>{{chatLabel .}}</td><td>{{.ChatID}}</td><td>{{.UpdatedAt.Format "2006-01-02 15:04"}}</td></tr>{{else}}<tr><td colspan="3">暂无群组</td></tr>{{end}}
</tbody></table></div>
</div>

<div class="card">
<h2>广播操作人</h2>
<p class="hint">宿主和默认操作人拥有全部广播权限；这里添加的是普通广播操作人，需要再授权分组或单群。</p>
<form method="post" action="/admin/operator/save" class="row">
<input name="user_id" placeholder="操作人 UID">
<input name="remark" placeholder="备注，可选">
<button class="btn" type="submit">保存</button>
</form>
<div class="scroll"><table><thead><tr><th>UID</th><th>备注</th><th>状态</th><th>操作</th></tr></thead><tbody>
{{range .BOperators}}<tr><td>{{.UserID}}</td><td>{{.Remark}}</td><td><span class="pill">{{.Status}}</span></td><td><form method="post" action="/admin/operator/disable"><input type="hidden" name="user_id" value="{{.UserID}}"><button class="btn mini" type="submit">禁用</button></form></td></tr>{{else}}<tr><td colspan="4">暂无广播操作人</td></tr>{{end}}
</tbody></table></div>
</div>

<div class="card wide">
<h2>广播分组</h2>
<p class="hint">先创建分组，再用下方多选框批量添加或移除群组。页面显示群名，数据库仍用群 ID 去重。</p>
<form method="post" action="/admin/group/save" class="row two">
<input name="name" placeholder="输入新分组名，例如 财务">
<button class="btn" type="submit">新建/更新分组</button>
</form>
<form method="post" action="/admin/group/delete" class="row two">
<select name="name">{{range .BGroups}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select>
<button class="btn" type="submit">删除分组</button>
</form>
<div class="row">
<form method="post" action="/admin/group/add">
<select class="full" name="name">{{range .BGroups}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select>
<select class="full" name="chat_id" multiple>{{range .Groups}}<option value="{{.ChatID}}">{{chatLabel .}}</option>{{end}}</select>
<button class="btn full" type="submit">批量添加群组</button>
</form>
<form method="post" action="/admin/group/remove">
<select class="full" name="name">{{range .BGroups}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select>
<select class="full" name="chat_id" multiple>{{range .Groups}}<option value="{{.ChatID}}">{{chatLabel .}}</option>{{end}}</select>
<button class="btn full" type="submit">批量移除群组</button>
</form>
<div class="scroll"><table><thead><tr><th>分组</th><th>群数</th><th>群组</th></tr></thead><tbody>
{{range .BGroups}}<tr><td>{{.Name}}</td><td>{{len .ChatIDs}}</td><td>{{range $i,$n := .ChatNames}}{{if $i}}、{{end}}{{$n}}{{end}}</td></tr>{{else}}<tr><td colspan="3">暂无广播分组</td></tr>{{end}}
</tbody></table></div>
</div>
</div>

<div class="card wide">
<h2>广播权限</h2>
<p class="hint">给普通广播操作人授权分组或单群。授权后，私聊机器人选择群发、分组广播或单群发送时只会看到允许的目标。</p>
<form method="post" action="/admin/permission/grant" class="row">
<select name="user_id">{{range .BOperators}}<option value="{{.UserID}}">{{.UserID}} {{.Remark}}</option>{{end}}</select>
<select name="target"><option value="group">授权分组</option><option value="chat">授权单群</option></select>
<select name="group_name">{{range .BGroups}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select>
<select name="chat_id">{{range .Groups}}<option value="{{.ChatID}}">{{chatLabel .}}</option>{{end}}</select>
<button class="btn" type="submit">授权</button>
</form>
<form method="post" action="/admin/permission/revoke" class="row">
<select name="user_id">{{range .BOperators}}<option value="{{.UserID}}">{{.UserID}} {{.Remark}}</option>{{end}}</select>
<select name="target"><option value="group">取消分组</option><option value="chat">取消单群</option></select>
<select name="group_name">{{range .BGroups}}<option value="{{.Name}}">{{.Name}}</option>{{end}}</select>
<select name="chat_id">{{range .Groups}}<option value="{{.ChatID}}">{{chatLabel .}}</option>{{end}}</select>
<button class="btn" type="submit">取消授权</button>
</form>
<div class="scroll"><table><thead><tr><th>UID</th><th>权限</th><th>授权人</th></tr></thead><tbody>
{{range .Permissions}}<tr><td>{{.UserID}}</td><td>{{permissionTarget . $.ChatNames}}</td><td>{{.GrantedBy}}</td></tr>{{else}}<tr><td colspan="3">暂无权限</td></tr>{{end}}
</tbody></table></div>
</div>

<div class="card wide">
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
</section>
</main></body></html>`

const billHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>完整账单</title>
<style>
:root{--bg:#eaf1f7;--panel:#fff;--line:#d8e2ee;--ink:#0e1b2f;--muted:#5b6f88;--gold:#d8b45d;--blue:#244f9f;--deep:#14223a}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font-family:Arial,"Microsoft YaHei",sans-serif}
.wrap{max-width:1120px;margin:0 auto;padding:26px}.head{display:flex;justify-content:space-between;gap:16px;align-items:flex-end;margin-bottom:18px}
.brand{color:#b97914;font-weight:800}.title{font-size:30px;font-weight:900;margin-top:8px}.sub{color:var(--muted);margin-top:6px}
.tools{display:flex;gap:10px;align-items:center;flex-wrap:wrap}.btn{height:38px;border:1px solid #c8d6e6;border-radius:6px;background:#fff;color:var(--ink);font-weight:800;padding:0 14px;text-decoration:none;display:inline-flex;align-items:center;justify-content:center;cursor:pointer}.btn.primary{background:var(--deep);color:#fff;border-color:var(--deep)}.history{height:38px;border:1px solid #c8d6e6;border-radius:6px;background:#fff;padding:0 10px;font-weight:800;color:var(--ink)}
.stats{display:grid;grid-template-columns:repeat(6,minmax(0,1fr));gap:10px;margin:0 0 16px}.stat{background:var(--panel);border:1px solid #c8d6e6;border-radius:8px;padding:14px 16px;min-height:76px}.stat span{display:block;color:var(--muted);font-size:13px}.stat strong{display:block;margin-top:8px;font-size:19px;line-height:1.25}
.filter-panel{background:var(--panel);border:1px solid #c8d6e6;border-radius:8px;padding:12px;margin-bottom:20px}.filter{display:grid;grid-template-columns:minmax(240px,1fr) 140px auto;gap:10px;margin:0}.filter input,.filter select{height:38px;border:1px solid #c8d6e6;border-radius:6px;background:#fff;color:var(--ink);padding:0 12px;font-size:14px}.filter button{border:0}
.bill-card{background:var(--panel);border:1px solid #c8d6e6;border-top:5px solid var(--gold);border-radius:8px;padding:18px 18px 14px;margin-top:16px}.bill-card h2{font-size:22px;margin:4px 0 16px}.bill-card h2 small{font-size:14px;color:var(--muted);font-weight:700}.table-scroll{overflow-x:auto}
table{width:100%;border-collapse:collapse;table-layout:fixed}th,td{border:1px solid var(--line);padding:11px 12px;text-align:center;vertical-align:middle;word-break:break-word}th{background:#f5f8fb;font-size:15px}.time{width:150px}.amount-col{width:260px}.actor{width:260px}.remark{width:auto}
.amount{color:var(--blue);font-weight:700}.empty{padding:36px;text-align:center;color:var(--muted)}.totals{display:flex;gap:18px;flex-wrap:wrap;margin-top:14px;color:var(--muted);font-size:14px}.totals strong{color:var(--ink)}
@media(max-width:900px){.stats{grid-template-columns:repeat(2,minmax(0,1fr))}.wrap{padding:18px}.head{display:block}.tools{margin-top:14px}}
@media(max-width:720px){.wrap{padding:14px}.btn,.history{width:100%}.filter{grid-template-columns:1fr}.filter button{width:100%}table{font-size:13px;min-width:760px}th,td{padding:9px}.stats{grid-template-columns:1fr}.title{font-size:26px}}
</style>
</head>
<body><main class="wrap">
<section class="head">
<div><div class="brand">Telegram 记账机器人</div><div class="title">{{.Group.Title}}</div><div class="sub">群 ID：{{.Group.ChatID}} · {{.DayKey}} · 北京时间</div></div>
<div class="tools">
<a class="btn" href="{{.PrevPath}}">上一天</a>
<select class="history" onchange="if(this.value){location.href=this.value}">
<option value="">历史账单</option>
{{range .BillDays}}<option value="/b/{{$.Group.ChatID}}/{{.}}{{$.FilterSuffix}}" {{if eq . $.DayKey}}selected{{end}}>{{.}}</option>{{end}}
</select>
<a class="btn" href="{{.NextPath}}">下一天</a>
<a class="btn primary" href="{{.DownloadPath}}">下载账单</a>
</div>
</section>
<section class="stats">
<div class="stat"><span>总入款</span><strong>{{.Summary.TotalDepositCNY}} / {{.Summary.TotalDepositUSDT}}U</strong></div>
<div class="stat"><span>汇率</span><strong>{{.Summary.ExchangeRate}}</strong></div>
<div class="stat"><span>交易费率</span><strong>{{.Summary.FeeRate}}%</strong></div>
<div class="stat"><span>应下发</span><strong>{{.Summary.TotalDepositUSDT}}U</strong></div>
<div class="stat"><span>已下发</span><strong>{{.Summary.TotalPayoutUSDT}}U</strong></div>
<div class="stat"><span>余额</span><strong>{{.Summary.BalanceUSDT}}U</strong></div>
</section>
<section class="filter-panel">
<form class="filter" method="get" action="/b/{{.Group.ChatID}}/{{.DayKey}}">
<input name="q" value="{{.Query}}" placeholder="输入名字、备注、金额或时间关键词">
<select name="field">
<option value="all" {{if eq .Field "all"}}selected{{end}}>全部字段</option>
<option value="actor" {{if eq .Field "actor"}}selected{{end}}>按操作人</option>
<option value="remark" {{if eq .Field "remark"}}selected{{end}}>按备注</option>
<option value="amount" {{if eq .Field "amount"}}selected{{end}}>按金额</option>
</select>
<button class="btn primary" type="submit">搜索</button>
</form>
</section>
<section class="bill-card">
<h2>入款 <small>({{.Summary.DepositCount}}笔)</small></h2>
<div class="table-scroll"><table><thead><tr><th class="time">时间</th><th class="amount-col">金额</th><th class="actor">操作人</th><th class="remark">备注</th></tr></thead><tbody>
{{range .Summary.Deposits}}<tr><td>{{.CreatedAt.Format "01-02 15:04:05"}}</td><td class="amount">{{billAmount .}}</td><td>{{.ActorName}}</td><td>{{.Remark}}</td></tr>{{else}}<tr><td class="empty" colspan="4">暂无入款记录</td></tr>{{end}}
</tbody></table></div>
<div class="totals"><span>总入款：<strong>{{.Summary.TotalDepositCNY}} / {{.Summary.TotalDepositUSDT}}U</strong></span><span>汇率：<strong>{{.Summary.ExchangeRate}}</strong></span><span>交易费率：<strong>{{.Summary.FeeRate}}%</strong></span></div>
</section>
<section class="bill-card">
<h2>下发 <small>({{.Summary.PayoutCount}}笔)</small></h2>
<div class="table-scroll"><table><thead><tr><th class="time">时间</th><th class="amount-col">金额</th><th class="actor">操作人</th><th class="remark">备注</th></tr></thead><tbody>
{{range .Summary.Payouts}}<tr><td>{{.CreatedAt.Format "01-02 15:04:05"}}</td><td class="amount">{{billAmount .}}</td><td>{{.ActorName}}</td><td>{{.Remark}}</td></tr>{{else}}<tr><td class="empty" colspan="4">暂无下发记录</td></tr>{{end}}
</tbody></table></div>
<div class="totals"><span>应下发：<strong>{{.Summary.TotalDepositUSDT}}U</strong></span><span>已下发：<strong>{{.Summary.TotalPayoutUSDT}}U</strong></span><span>余额：<strong>{{.Summary.BalanceUSDT}}U</strong></span></div>
</section>
</main></body></html>`
