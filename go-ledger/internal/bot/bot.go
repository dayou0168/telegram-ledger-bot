package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainclient"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/permissions"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

type Bot struct {
	cfg           config.Config
	store         *storage.Store
	fallbackStore *storage.Store
	tg            *telegram.Client
	tron          *tron.Client
	fallbackTron  *tron.Client
	p2p           *p2p.Client
	watcher       *chainclient.Client
	perms         permissions.Policy
	loc           *time.Location

	dispatcher      *worker.Dispatcher
	updateAdmission *updateAdmission
	ledgerPool      *worker.Pool
	controlPool     *worker.Pool
	chainPool       *worker.Pool
	ratePool        *worker.Pool
	broadcastPool   *worker.Pool
	queryPool       *worker.Pool
	notifyPool      *worker.Pool
	quickReplyPool  *worker.Pool
	criticalPool    *worker.Pool
	quickReplyLease time.Duration

	groupTouchCache       *ttlCache[string]
	groupCache            *ttlCache[storage.Group]
	billSummaryCache      *ttlCache[storage.BillSummaryData]
	userTouchCache        *ttlCache[string]
	operatorCache         *ttlCache[bool]
	globalCapabilityCache *globalCapabilityCache
	watchTargetCache      *ttlCache[[]storage.WatchTarget]
	fallbackSubCache      *ttlCache[[]storage.ChainWatcherSubscription]
	rateBookCache         *ttlCache[[]p2p.OrderBookEntry]
	rateBookState         rateBookState
	privateStates         *ttlCache[privateState]
	notificationWake      chan struct{}
	quickReplyWake        chan struct{}
	criticalOutboxWake    chan int64
	telegramLedgerWake    chan struct{}
	telegramBypassWake    chan struct{}
	telegramInboxLease    time.Duration
	telegramLimiter       *telegramRateLimiter
	sendGateway           *telegramSendGateway
	watchRunning          atomic.Bool
	watcherFallback       *watcherFallbackController
	watcherTiming         chainWatcherTimingStatus
	fallbackNextPoll      atomic.Int64
	fallbackBackoff       atomic.Int32
	fallbackLeaderActive  atomic.Bool
	globalPermissionEpoch atomic.Int64
	telegramInboxStats    atomic.Value
	ledgerSummaryCompare  atomic.Uint64
	globalOperatorLookup  func(context.Context, int64) (permissions.UserCapabilities, bool, error)
	permissionEpochLookup func(context.Context) (int64, error)
	groupOperatorLookup   func(context.Context, int64, int64) (bool, error)
	updateHandler         func(context.Context, telegram.Update) error
	inboxMarkHandled      func(context.Context, storage.TelegramInboxUpdate, string, time.Time) (bool, error)
	inboxComplete         func(context.Context, storage.TelegramInboxUpdate, string, time.Time) (bool, error)
}

const privateStateTTL = 30 * time.Minute

func (b *Bot) executeUpdate(ctx context.Context, update telegram.Update) error {
	if b.updateHandler != nil {
		return b.updateHandler(ctx, update)
	}
	return b.handleUpdate(ctx, update)
}

func New(cfg config.Config, store *storage.Store, tg *telegram.Client, tronClient *tron.Client, p2pClient *p2p.Client, fallbackStores ...*storage.Store) *Bot {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.FixedZone("Asia/Shanghai", 8*3600)
	}
	var fallbackStore *storage.Store
	if len(fallbackStores) > 0 {
		fallbackStore = fallbackStores[0]
	}
	criticalWorkers := cfg.NotifyWorkers / 2
	if criticalWorkers < 2 {
		criticalWorkers = 2
	}
	bot := &Bot{
		cfg:                   cfg,
		store:                 store,
		fallbackStore:         fallbackStore,
		tg:                    tg,
		tron:                  tronClient,
		fallbackTron:          tron.NewPublicFallbackClient(cfg.TronAPIBase, cfg.RequestTimeout),
		p2p:                   p2pClient,
		watcher:               chainclient.New(cfg.ChainWatcherURL, cfg.ChainWatcherBotID, cfg.ChainWatcherSecret, cfg.RequestTimeout),
		perms:                 permissions.NewPolicy(cfg.HostUserID, cfg.DefaultOperatorIDs),
		loc:                   loc,
		dispatcher:            worker.NewDispatcher(cfg.QueueSize),
		updateAdmission:       newUpdateAdmission(cfg.QueueSize),
		ledgerPool:            worker.NewPool("ledger", cfg.LedgerWorkers, cfg.QueueSize),
		controlPool:           worker.NewPool("control", cfg.ControlWorkers, cfg.QueueSize),
		chainPool:             worker.NewPool("chain", cfg.ChainWorkers, cfg.QueueSize),
		ratePool:              worker.NewPool("rate", cfg.RateWorkers, cfg.QueueSize),
		broadcastPool:         worker.NewPool("broadcast", cfg.BroadcastWorkers, cfg.QueueSize),
		queryPool:             worker.NewPool("query", cfg.QueryWorkers, cfg.QueueSize),
		notifyPool:            worker.NewPool("notify", cfg.NotifyWorkers, cfg.QueueSize),
		quickReplyPool:        worker.NewPool("quick-reply", cfg.NotifyWorkers, cfg.QueueSize),
		criticalPool:          worker.NewPool("critical-notify", criticalWorkers, cfg.QueueSize),
		quickReplyLease:       quickReplyOutboxLease,
		groupTouchCache:       newTTLCache[string](cfg.GroupCacheTTL),
		groupCache:            newTTLCache[storage.Group](cfg.GroupCacheTTL),
		billSummaryCache:      newTTLCache[storage.BillSummaryData](cfg.BillSummaryCacheTTL),
		userTouchCache:        newTTLCache[string](cfg.UserTouchCacheTTL),
		operatorCache:         newTTLCache[bool](cfg.OperatorCacheTTL),
		globalCapabilityCache: newGlobalCapabilityCache(cfg.GlobalPermissionCacheTTL, cfg.GlobalPermissionCacheSize),
		watchTargetCache:      newTTLCache[[]storage.WatchTarget](cfg.WatchCacheTTL),
		fallbackSubCache:      newTTLCache[[]storage.ChainWatcherSubscription](cfg.WatchCacheTTL),
		rateBookCache:         newTTLCache[[]p2p.OrderBookEntry](cfg.P2PCacheTTL),
		privateStates:         newTTLCache[privateState](privateStateTTL),
		notificationWake:      make(chan struct{}, 1),
		quickReplyWake:        make(chan struct{}, 1),
		criticalOutboxWake:    make(chan int64, cfg.QueueSize),
		telegramLedgerWake:    make(chan struct{}, 1),
		telegramBypassWake:    make(chan struct{}, 1),
		telegramInboxLease:    telegramInboxLease,
		telegramLimiter:       newTelegramRateLimiter(),
		watcherFallback:       newWatcherFallbackControllerWithRecovery(cfg.BotWatcherFailThreshold, cfg.BotFallbackRecoverySuccesses, cfg.BotFallbackRecoveryLag),
	}
	bot.fallbackTron.SetMinRequestInterval(cfg.BotFallbackRequestInterval)
	bot.sendGateway = newTelegramSendGateway(tg, bot.telegramLimiter, cfg.NotifyWorkers, cfg.QueueSize)
	return bot
}

func (b *Bot) Run(ctx context.Context) error {
	b.ledgerPool.StartN(ctx, b.cfg.LedgerWorkers)
	b.controlPool.StartN(ctx, b.cfg.ControlWorkers)
	b.chainPool.StartN(ctx, b.cfg.ChainWorkers)
	b.ratePool.StartN(ctx, b.cfg.RateWorkers)
	b.broadcastPool.StartN(ctx, b.cfg.BroadcastWorkers)
	b.queryPool.StartN(ctx, b.cfg.QueryWorkers)
	b.notifyPool.StartN(ctx, b.cfg.NotifyWorkers)
	b.quickReplyPool.StartN(ctx, b.cfg.NotifyWorkers)
	b.criticalPool.Start(ctx)
	b.sendGateway.Start(ctx)
	b.updateAdmission.Start(ctx)

	if b.cfg.ChainWatcherEnabled() {
		if b.fallbackStore == nil {
			log.Printf("DEGRADED: BOT_FALLBACK_SHARED_DATABASE_URL is not configured; shared public fallback is unavailable")
		}
		go b.chainWatcherSyncScheduler(ctx)
		go b.chainWatcherEventScheduler(ctx)
		go b.chainWatcherHealthScheduler(ctx)
		go b.chainWatcherFallbackScheduler(ctx)
	}
	go b.notificationOutboxScheduler(ctx)
	go b.quickReplyOutboxScheduler(ctx)
	go b.criticalOutboxScheduler(ctx)
	go b.ledgerSummaryReconcileScheduler(ctx)
	go b.privateCleanupScheduler(ctx)
	go b.rateScheduler(ctx)
	go b.permissionInvalidationScheduler(ctx)
	b.startTelegramInbox(ctx)

	offset, err := b.store.GetTelegramPollOffset(ctx, b.telegramInboxStreamKey())
	if err != nil {
		return fmt.Errorf("load telegram poll offset: %w", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		updates, err := b.tg.GetUpdates(ctx, offset, b.cfg.PollTimeout)
		if err != nil {
			log.Printf("get updates: %v", err)
			time.Sleep(time.Second)
			continue
		}
		if len(updates) == 0 {
			continue
		}
		offset, err = b.persistTelegramUpdateBatch(ctx, updates)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("persist telegram update batch: %v", err)
			time.Sleep(time.Second)
			continue
		}
		b.wakeTelegramInbox()
	}
}

func (b *Bot) telegramInboxStreamKey() string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(b.cfg.TelegramBotToken)))
	return "bot:" + hex.EncodeToString(sum[:16])
}

func updateChatID(update telegram.Update) int64 {
	if update.Message != nil {
		return update.Message.Chat.ID
	}
	if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
		return update.CallbackQuery.Message.Chat.ID
	}
	if update.MyChatMember != nil {
		return update.MyChatMember.Chat.ID
	}
	if update.ChatMember != nil {
		return update.ChatMember.Chat.ID
	}
	return 0
}

func classifyMessageCommand(text string, chatType string) string {
	text = strings.TrimSpace(text)
	if chatType == "private" {
		if text == "" || text == "/start" || text == "菜单" {
			return "private_menu"
		}
		if _, ok := parseTRXAddressQuery(text); ok {
			return "private_trx_query"
		}
		return "private_message"
	}
	switch text {
	case "":
		return "non_text"
	case "开始":
		return "ledger_start"
	case "停止", "关闭":
		return "ledger_stop"
	case "上课", "下课":
		return "business_mode"
	}
	if isBillCommand(text) {
		return "ledger_bill"
	}
	if isZ0Command(text) {
		return "z0"
	}
	if _, ok := parseZRateSetting(text); ok {
		return "zrate_setting"
	}
	if _, ok := parseSetting(text); ok {
		return "ledger_setting"
	}
	if _, ok := parseClearScope(text); ok {
		return "ledger_clear"
	}
	if _, ok := parseUndoKind(text); ok {
		return "ledger_undo"
	}
	if isNotifyAllCommand(text) {
		return "notify_all"
	}
	if isOperatorWriteCommand(text) {
		return "operator_write"
	}
	if isOperatorListCommand(text) {
		return "operator_list"
	}
	if _, ok := parseTRXAddressQuery(text); ok {
		return "trx_query"
	}
	if isTRC20Address(text) {
		return "address_validation"
	}
	if _, ok := parseLedger(text); ok {
		return "ledger_record"
	}
	if isArithmeticExpression(text) {
		return "calculator"
	}
	return "other"
}

func (b *Bot) updateRoute(update telegram.Update) (string, worker.Executor) {
	if update.Message != nil {
		if update.Message.Chat.Type == "private" {
			userID := int64(0)
			if update.Message.From != nil {
				userID = update.Message.From.ID
			}
			return "private:" + strconv.FormatInt(userID, 10), b.controlPool
		}
		chatID := update.Message.Chat.ID
		if b.cfg.GroupRouteMode == "split" {
			command := classifyMessageCommand(update.Message.TextOrCaption(), update.Message.Chat.Type)
			switch command {
			case "other", "non_text", "calculator", "z0":
				return fmt.Sprintf("bypass:%d:%d", chatID, update.UpdateID), b.queryPool
			}
		}
		return "ledger:" + strconv.FormatInt(chatID, 10), b.ledgerPool
	}
	if update.CallbackQuery != nil {
		if update.CallbackQuery.Message != nil && update.CallbackQuery.Message.Chat.Type == "private" {
			return "private:" + strconv.FormatInt(update.CallbackQuery.From.ID, 10), b.controlPool
		}
		if update.CallbackQuery.Message != nil {
			return "ledger:" + strconv.FormatInt(update.CallbackQuery.Message.Chat.ID, 10), b.ledgerPool
		}
		return "private:" + strconv.FormatInt(update.CallbackQuery.From.ID, 10), b.controlPool
	}
	if update.MyChatMember != nil {
		return "ledger:" + strconv.FormatInt(update.MyChatMember.Chat.ID, 10), b.ledgerPool
	}
	if update.ChatMember != nil {
		return "ledger:" + strconv.FormatInt(update.ChatMember.Chat.ID, 10), b.ledgerPool
	}
	return "update:" + strconv.FormatInt(update.UpdateID, 10), b.controlPool
}

func (b *Bot) handleUpdate(ctx context.Context, update telegram.Update) error {
	ctx = contextWithPermissionMemo(ctx)
	ctx = context.WithValue(ctx, telegramUpdateIDContextKey{}, update.UpdateID)
	switch {
	case update.Message != nil:
		return b.handleMessage(ctx, *update.Message)
	case update.CallbackQuery != nil:
		return b.handleCallback(ctx, *update.CallbackQuery)
	case update.MyChatMember != nil:
		return b.handleMyChatMember(ctx, *update.MyChatMember)
	case update.ChatMember != nil:
		return b.handleChatMember(ctx, *update.ChatMember)
	default:
		return nil
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg telegram.Message) error {
	if msg.From == nil {
		return nil
	}
	user := userFromTelegram(*msg.From)
	now := telegramExecutionTime(b.loc)
	eventTime := telegramUpdateEventTime(ctx, b.loc)
	text := strings.TrimSpace(msg.TextOrCaption())
	doneParse := measurePerfStage(ctx, "command_parse")
	command := classifyMessageCommand(text, msg.Chat.Type)
	doneParse()
	setPerfCommand(ctx, command)
	if msg.Chat.Type == "private" {
		return b.handlePrivateMessage(ctx, msg, user, text, now, eventTime)
	}
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return nil
	}
	if err := b.ensureGroupCached(ctx, msg.Chat.ID, msg.Chat.Title, eventTime); err != nil {
		return err
	}
	if err := b.touchUserCached(ctx, msg.Chat.ID, user, eventTime); err != nil {
		return err
	}
	if err := b.observeMessageUsers(ctx, msg, user.ID, eventTime); err != nil {
		return err
	}
	if msg.ReplyTo != nil {
		b.notifyBroadcastReplyAsync(ctx, msg, user)
	}
	if text == "" {
		return nil
	}
	switch text {
	case "开始":
		return b.startAccounting(ctx, msg, user, now)
	case "停止", "关闭":
		return b.stopAccounting(ctx, msg, user, now)
	case "上课":
		return b.handleBusinessMode(ctx, msg, user, true, now)
	case "下课":
		return b.handleBusinessMode(ctx, msg, user, false, now)
	}
	if isNotifyAllCommand(text) {
		return b.handleNotifyAll(ctx, msg, user)
	}
	if isBillCommand(text) {
		if isOpenBillCommand(text) {
			return b.sendBill(ctx, msg.Chat.ID, msg.MessageID, now, "")
		}
		group, err := b.getGroupCached(ctx, msg.Chat.ID)
		if err != nil {
			return err
		}
		ok, err := b.guardAccountingStarted(ctx, msg, user, group, now, "ledger_bill_inactive")
		if err != nil || !ok {
			return err
		}
		return b.sendBill(ctx, msg.Chat.ID, msg.MessageID, now, "")
	}
	if isZ0Command(text) {
		return b.handleZ0(ctx, msg)
	}
	if cmd, ok := parseZRateSetting(text); ok {
		return b.handleZRateSetting(ctx, msg, user, cmd, now)
	}
	if cmd, ok := parseSetting(text); ok {
		return b.handleSetting(ctx, msg, user, cmd, now)
	}
	if scope, ok := parseClearScope(text); ok {
		return b.handleClearLedgerRequest(ctx, msg, user, scope)
	}
	if kind, ok := parseUndoKind(text); ok {
		return b.handleUndo(ctx, msg, user, kind, now)
	}
	if isOperatorWriteCommand(text) {
		return b.handleOperatorCommand(ctx, msg, user, text, now)
	}
	if isOperatorListCommand(text) {
		return b.handleListOperators(ctx, msg)
	}
	if address, ok := parseTRXAddressQuery(text); ok {
		return b.handleTRXAddressQuery(ctx, msg, address)
	}
	if isTRC20Address(text) {
		return b.handleAddressValidation(ctx, msg, user, text, now)
	}
	cmd, ok := parseLedger(text)
	if !ok {
		if isArithmeticExpression(text) {
			result, err := calculateExpression(text)
			if err != nil {
				return nil
			}
			return b.enqueueReliableText(ctx, sendPriorityNormal, "calc_result", messageScopedDedupe("calc_result", msg.Chat.ID, msg.MessageID), msg.Chat.ID, strings.TrimSpace(text)+"="+formatCalculationResult(result), nil, reliableMessageRef{}, now)
		}
		return nil
	}
	return b.handleLedger(ctx, msg, user, cmd, now)
}

func (b *Bot) handlePrivateMessage(ctx context.Context, msg telegram.Message, user storage.User, text string, now, eventTime time.Time) error {
	if err := b.touchUserCached(ctx, msg.Chat.ID, user, eventTime); err != nil {
		return err
	}
	b.recordIncomingPrivateChatMessage(ctx, msg, user, eventTime)
	if msg.UsersShared != nil {
		return b.handleUsersShared(ctx, msg)
	}
	if state, ok := b.privateStates.Get(formatID(user.ID)); ok {
		switch state.Mode {
		case "quick_reply":
			return b.handleQuickReplyMaterial(ctx, msg, user, state)
		case "watch_add", "watch_remove", "watch_min", "watch_target_min", "watch_target_label":
			if text == "菜单" || text == "/start" || text == "返回" || text == "取消" {
				b.privateStates.Delete(formatID(user.ID))
				return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
			}
			return b.handleAddressWatchState(ctx, msg, user, state, text, now)
		default:
			return b.handleBroadcastMaterial(ctx, msg, user, state, now)
		}
	}
	if text == "/start" || text == "菜单" || text == "" {
		return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
	}
	if text == "我的ID" || strings.EqualFold(text, "id") || text == "/id" {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "private_my_id", msg.Chat.ID, msg.MessageID, fmt.Sprintf("你的 Telegram ID：%d", user.ID), nil, now)
	}
	if address, ok := parseTRXAddressQuery(text); ok {
		return b.handleTRXAddressQuery(ctx, msg, address)
	}
	if text == "🔔地址监听" || text == "地址监听" || text == "监听地址" {
		return b.sendAddressWatchMenu(ctx, msg.Chat.ID, user.ID, msg.MessageID)
	}
	if handled, err := b.handlePrivateShortcut(ctx, msg, user, text); handled || err != nil {
		return err
	}
	if isBroadcastMenuText(text) {
		return b.sendBroadcastMenu(ctx, msg, user, text)
	}
	if text == "取消" || text == "返回" {
		b.privateStates.Delete(formatID(user.ID))
		return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
	}
	if match := watchAddPattern.FindStringSubmatch(text); match != nil {
		return b.addWatchFromPrivate(ctx, msg, user, match[1], match[2], now)
	}
	if match := watchDelPattern.FindStringSubmatch(text); match != nil {
		return b.removeWatchFromPrivate(ctx, msg, user, match[1], now)
	}
	return b.sendPrivateMenu(ctx, msg.Chat.ID, msg.MessageID)
}

func (b *Bot) handleUsersShared(ctx context.Context, msg telegram.Message) error {
	if msg.UsersShared == nil || len(msg.UsersShared.Users) == 0 {
		return b.enqueueReplyText(ctx, sendPriorityNormal, "users_shared_empty", msg.Chat.ID, msg.MessageID, "没有获取到用户 UID。", nil, time.Now().In(b.loc))
	}
	var out strings.Builder
	out.WriteString("已获取用户 UID：")
	for _, shared := range msg.UsersShared.Users {
		out.WriteByte('\n')
		name := strings.TrimSpace(strings.TrimSpace(shared.FirstName + " " + shared.LastName))
		if name == "" && shared.Username != "" {
			name = "@" + shared.Username
		}
		if name != "" {
			out.WriteString(name)
			out.WriteString("：")
		}
		out.WriteString(formatID(shared.UserID))
	}
	return b.enqueueReplyText(ctx, sendPriorityNormal, "users_shared_result", msg.Chat.ID, msg.MessageID, out.String(), nil, time.Now().In(b.loc))
}

func (b *Bot) handleCallback(ctx context.Context, cb telegram.CallbackQuery) error {
	if strings.HasPrefix(cb.Data, "watch:") {
		return b.handleAddressWatchCallback(ctx, cb)
	}
	if strings.HasPrefix(cb.Data, "bc:") {
		return b.handleBroadcastCallback(ctx, cb)
	}
	if strings.HasPrefix(cb.Data, "br:") {
		return b.handleBroadcastReplyCallback(ctx, cb)
	}
	if strings.HasPrefix(cb.Data, "clear:") {
		return b.handleClearLedgerCallback(ctx, cb)
	}
	return b.tg.AnswerCallback(ctx, cb.ID, "Go 版按钮处理正在迁移中。")
}

func (b *Bot) handleMyChatMember(ctx context.Context, upd telegram.ChatMemberUpd) error {
	eventTime := telegramUpdateEventTime(ctx, b.loc)
	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	switch upd.NewChatMember.Status {
	case "member", "administrator", "restricted":
		canInvite, err := b.canInvite(ctx, upd.From.ID)
		if err != nil {
			return err
		}
		if !canInvite {
			_, _ = b.sendText(ctx, sendPriorityNormal, upd.Chat.ID, "邀请人没有授权，机器人将自动退出。", nil)
			return b.tg.LeaveChat(ctx, upd.Chat.ID)
		}
		if err := b.ensureGroupCached(ctx, upd.Chat.ID, upd.Chat.Title, eventTime); err != nil {
			return err
		}
		return nil
	}
	return nil
}

func (b *Bot) handleChatMember(ctx context.Context, upd telegram.ChatMemberUpd) error {
	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	eventTime := telegramUpdateEventTime(ctx, b.loc)
	if upd.Date > 0 {
		eventTime = time.Unix(upd.Date, 0).In(b.loc)
	}
	if err := b.ensureGroupCached(ctx, upd.Chat.ID, upd.Chat.Title, eventTime); err != nil {
		return err
	}
	switch upd.NewChatMember.Status {
	case "creator", "administrator", "member", "restricted":
		member := upd.NewChatMember.User
		if member.ID == 0 || member.IsBot {
			return nil
		}
		updateID, _ := ctx.Value(telegramUpdateIDContextKey{}).(int64)
		return b.store.ObserveUserForTelegramUpdate(ctx, upd.Chat.ID, userFromTelegram(member), updateID, eventTime)
	default:
		return nil
	}
}

// observeTelegramUpdateIdentityAtIngress persists group identities before the
// durable inbox can wait behind ledger or broadcast work. Handler-side writes
// remain as an idempotent fallback for updates queued before this version.
func (b *Bot) observeTelegramUpdateIdentityAtIngress(ctx context.Context, update telegram.Update, fallback time.Time) error {
	switch {
	case update.Message != nil:
		msg := *update.Message
		if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
			return nil
		}
		eventTime := fallback
		if msg.Date > 0 {
			eventTime = time.Unix(msg.Date, 0).In(b.loc)
		}
		if err := b.store.EnsureGroupForTelegramUpdate(ctx, msg.Chat.ID, msg.Chat.Title, update.UpdateID, eventTime); err != nil {
			return err
		}
		senderID := int64(0)
		if msg.From != nil && msg.From.ID != 0 && !msg.From.IsBot {
			senderID = msg.From.ID
			if err := b.store.TouchUserForTelegramUpdate(ctx, msg.Chat.ID, userFromTelegram(*msg.From), update.UpdateID, eventTime); err != nil {
				return err
			}
		}
		return b.observeMessageUsersForUpdate(ctx, msg, senderID, update.UpdateID, eventTime)
	case update.ChatMember != nil:
		upd := *update.ChatMember
		if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
			return nil
		}
		eventTime := fallback
		if upd.Date > 0 {
			eventTime = time.Unix(upd.Date, 0).In(b.loc)
		}
		if err := b.store.EnsureGroupForTelegramUpdate(ctx, upd.Chat.ID, upd.Chat.Title, update.UpdateID, eventTime); err != nil {
			return err
		}
		member := upd.NewChatMember.User
		if member.ID == 0 || member.IsBot {
			return nil
		}
		return b.store.ObserveUserForTelegramUpdate(ctx, upd.Chat.ID, userFromTelegram(member), update.UpdateID, eventTime)
	default:
		return nil
	}
}

func (b *Bot) observeMessageUsers(ctx context.Context, msg telegram.Message, senderID int64, eventTime time.Time) error {
	updateID, _ := ctx.Value(telegramUpdateIDContextKey{}).(int64)
	return b.observeMessageUsersForUpdate(ctx, msg, senderID, updateID, eventTime)
}

func (b *Bot) observeMessageUsersForUpdate(ctx context.Context, msg telegram.Message, senderID, updateID int64, eventTime time.Time) error {
	observed := make(map[int64]struct{})
	observe := func(candidate telegram.User, spoken bool, seenAt time.Time) error {
		if candidate.ID == 0 || candidate.ID == senderID || candidate.IsBot {
			return nil
		}
		if _, exists := observed[candidate.ID]; exists {
			return nil
		}
		observed[candidate.ID] = struct{}{}
		user := userFromTelegram(candidate)
		if spoken {
			return b.store.TouchUserForTelegramUpdate(ctx, msg.Chat.ID, user, updateID, seenAt)
		}
		return b.store.ObserveUserForTelegramUpdate(ctx, msg.Chat.ID, user, updateID, seenAt)
	}
	if msg.ReplyTo != nil && msg.ReplyTo.From != nil {
		seenAt := eventTime
		if msg.ReplyTo.Date > 0 {
			seenAt = time.Unix(msg.ReplyTo.Date, 0).In(b.loc)
		}
		if err := observe(*msg.ReplyTo.From, true, seenAt); err != nil {
			return err
		}
	}
	for _, member := range msg.NewChatMembers {
		if err := observe(member, false, eventTime); err != nil {
			return err
		}
	}
	observeEntities := func(entities []telegram.MessageEntity) error {
		for _, entity := range entities {
			if entity.Type == "text_mention" && entity.User != nil {
				if err := observe(*entity.User, false, eventTime); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := observeEntities(msg.Entities); err != nil {
		return err
	}
	if err := observeEntities(msg.CaptionEntities); err != nil {
		return err
	}
	return nil
}

func (b *Bot) sendPrivateMenu(ctx context.Context, chatID int64, replyTo int64) error {
	return b.enqueueReliableText(ctx, sendPriorityNormal, "private_menu", messageScopedDedupe("private_menu", chatID, replyTo), chatID, "请选择功能：", map[string]any{
		"reply_to_message_id": replyTo,
		"reply_markup":        privateMenuKeyboard(),
	}, reliableMessageRef{}, time.Now().In(b.loc))
}

func privateMenuKeyboard() telegram.ReplyKeyboardMarkup {
	return telegram.ReplyKeyboardMarkup{
		Keyboard: [][]telegram.KeyboardButton{
			{{Text: "✍开始记账"}, {Text: "📃详细说明"}},
			{{Text: "📡群发广播"}, {Text: "🔔地址监听"}},
			{{Text: "🔎查询UID"}, {Text: "⚙后台管理"}},
		},
		ResizeKeyboard: true,
	}
}

func (b *Bot) sendGroupMessageAsync(ctx context.Context, chatID int64, text string, replyTo int64) {
	key := "send:" + strconv.FormatInt(chatID, 10)
	b.dispatcher.Submit(ctx, key, b.notifyPool, func(sendCtx context.Context) {
		opts := map[string]any{}
		if replyTo > 0 {
			opts["reply_to_message_id"] = replyTo
		}
		dedupeKey := messageScopedDedupe("group_message", chatID, replyTo)
		if replyTo == 0 {
			dedupeKey = fmt.Sprintf("group_message:%d:%d", chatID, time.Now().UnixNano())
		}
		if err := b.enqueueReliableText(sendCtx, sendPriorityNormal, "group_message", dedupeKey, chatID, text, opts, reliableMessageRef{}, time.Now().In(b.loc)); err != nil {
			log.Printf("enqueue group message %d: %v", chatID, err)
		}
	})
}

func userFromTelegram(u telegram.User) storage.User {
	display := u.DisplayName()
	if display == "" {
		display = strconv.FormatInt(u.ID, 10)
	}
	return storage.User{ID: u.ID, Username: u.Username, DisplayName: display}
}

func (b *Bot) ensureGroupCached(ctx context.Context, chatID int64, title string, now time.Time) error {
	updateID, _ := ctx.Value(telegramUpdateIDContextKey{}).(int64)
	if updateID > 0 {
		markPerfCache(ctx, "group_touch", false)
		done := measurePerfStage(ctx, "db_group_touch")
		defer done()
		return b.store.EnsureGroupForTelegramUpdate(ctx, chatID, title, updateID, now)
	}
	key := strconv.FormatInt(chatID, 10)
	if cached, ok := b.groupTouchCache.Get(key); ok && cached == title {
		markPerfCache(ctx, "group_touch", true)
		return nil
	}
	markPerfCache(ctx, "group_touch", false)
	done := measurePerfStage(ctx, "db_group_touch")
	defer done()
	if err := b.store.EnsureGroup(ctx, chatID, title, now); err != nil {
		return err
	}
	b.groupTouchCache.Set(key, title)
	return nil
}

func (b *Bot) touchUserCached(ctx context.Context, chatID int64, user storage.User, now time.Time) error {
	updateID, _ := ctx.Value(telegramUpdateIDContextKey{}).(int64)
	if updateID > 0 {
		markPerfCache(ctx, "user_touch", false)
		done := measurePerfStage(ctx, "db_user_touch")
		defer done()
		return b.store.TouchUserForTelegramUpdate(ctx, chatID, user, updateID, now)
	}
	key := fmt.Sprintf("%d:%d", chatID, user.ID)
	value := user.Username + "|" + user.DisplayName
	if cached, ok := b.userTouchCache.Get(key); ok && cached == value {
		markPerfCache(ctx, "user_touch", true)
		return nil
	}
	markPerfCache(ctx, "user_touch", false)
	done := measurePerfStage(ctx, "db_user_touch")
	defer done()
	if err := b.store.TouchUser(ctx, chatID, user, now); err != nil {
		return err
	}
	b.userTouchCache.Set(key, value)
	return nil
}

type telegramUpdateIDContextKey struct{}
type telegramUpdateReceivedAtContextKey struct{}

func telegramUpdateEventTime(ctx context.Context, loc *time.Location) time.Time {
	if value, ok := ctx.Value(telegramUpdateReceivedAtContextKey{}).(time.Time); ok && !value.IsZero() {
		return value.In(loc)
	}
	return telegramExecutionTime(loc)
}

func telegramExecutionTime(loc *time.Location) time.Time {
	now := time.Now()
	if loc != nil {
		return now.In(loc)
	}
	return now
}

func (b *Bot) getGroupCached(ctx context.Context, chatID int64) (storage.Group, error) {
	key := strconv.FormatInt(chatID, 10)
	if group, ok := b.groupCache.Get(key); ok {
		markPerfCache(ctx, "group", true)
		return group, nil
	}
	markPerfCache(ctx, "group", false)
	done := measurePerfStage(ctx, "db_group_get")
	defer done()
	group, err := b.store.GetGroup(ctx, chatID)
	if err != nil {
		return storage.Group{}, err
	}
	b.groupCache.Set(key, group)
	return group, nil
}

func (b *Bot) invalidateGroupCache(chatID int64) {
	if b.groupCache != nil {
		b.groupCache.Delete(strconv.FormatInt(chatID, 10))
	}
}

func (b *Bot) getBillSummaryCached(ctx context.Context, group storage.Group, dayKey string, limit int) (storage.BillSummaryData, error) {
	chatID := group.ChatID
	periodStart := group.ActivePeriodStartedAt
	if b.billSummaryCache == nil {
		return b.getBillSummaryForPeriod(ctx, chatID, dayKey, periodStart, limit)
	}
	key := billSummaryCacheKey(chatID, dayKey, periodStart, limit)
	if data, ok := b.billSummaryCache.Get(key); ok {
		markPerfCache(ctx, "bill_summary", true)
		return data, nil
	}
	markPerfCache(ctx, "bill_summary", false)
	data, err := b.getBillSummaryForPeriod(ctx, chatID, dayKey, periodStart, limit)
	if err != nil {
		return storage.BillSummaryData{}, err
	}
	b.billSummaryCache.Set(key, data)
	return data, nil
}

func (b *Bot) invalidateBillSummaryCache(chatID int64, dayKey string) {
	if b.billSummaryCache != nil {
		b.billSummaryCache.DeletePrefix(strconv.FormatInt(chatID, 10) + ":" + dayKey + ":")
	}
}

func (b *Bot) clearBillSummaryCache() {
	if b.billSummaryCache != nil {
		b.billSummaryCache.Clear()
	}
}

func billSummaryCacheKey(chatID int64, dayKey string, periodStart time.Time, limit int) string {
	return strconv.FormatInt(chatID, 10) + ":" + dayKey + ":" + strconv.FormatInt(periodStart.UnixNano(), 10) + ":" + strconv.Itoa(limit)
}

func (b *Bot) getBillSummaryForPeriod(ctx context.Context, chatID int64, dayKey string, periodStart time.Time, limit int) (storage.BillSummaryData, error) {
	useSummary := b.cfg.LedgerSummaryWriteMode == "shadow" && b.cfg.LedgerSummaryReadMode == "safe"
	compare := false
	if useSummary && b.cfg.LedgerSummaryCompareEvery > 0 {
		compare = b.ledgerSummaryCompare.Add(1)%uint64(b.cfg.LedgerSummaryCompareEvery) == 0
	}
	return b.store.GetBillSummaryForPeriod(ctx, storage.LedgerPeriodSummaryKey{
		ChatID: chatID, DayKey: dayKey, PeriodStartedAt: periodStart,
	}, limit, useSummary, compare, time.Now().In(b.loc))
}

func (b *Bot) canInvite(ctx context.Context, userID int64) (bool, error) {
	if userID == 0 {
		return false, nil
	}
	if b.perms.CanInviteBot(userID, permissions.UserCapabilities{}) {
		return true, nil
	}
	caps, ok, err := b.globalOperatorCapabilities(ctx, userID)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	return b.perms.CanInviteBot(userID, caps), nil
}

func (b *Bot) isHost(userID int64) bool {
	return b.perms.IsHost(userID)
}

func (b *Bot) isRoot(userID int64) bool {
	return b.isPrivileged(userID)
}

func (b *Bot) isPrivileged(userID int64) bool {
	return b.perms.IsPrivileged(userID)
}
