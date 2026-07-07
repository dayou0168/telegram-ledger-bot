package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

type Bot struct {
	cfg   config.Config
	store *storage.Store
	tg    *telegram.Client
	tron  *tron.Client
	p2p   *p2p.Client
	loc   *time.Location

	dispatcher    *worker.Dispatcher
	ledgerPool    *worker.Pool
	controlPool   *worker.Pool
	chainPool     *worker.Pool
	ratePool      *worker.Pool
	broadcastPool *worker.Pool
	queryPool     *worker.Pool
	notifyPool    *worker.Pool

	groupTouchCache  *ttlCache[string]
	userTouchCache   *ttlCache[string]
	operatorCache    *ttlCache[bool]
	watchTargetCache *ttlCache[[]storage.WatchTarget]
	rateBookCache    *ttlCache[[]p2p.OrderBookEntry]
	privateStates    *ttlCache[privateState]
	notificationWake chan struct{}
	telegramLimiter  *telegramRateLimiter
	textGateway      *telegramTextGateway
	watchRunning     atomic.Bool
}

func New(cfg config.Config, store *storage.Store, tg *telegram.Client, tronClient *tron.Client, p2pClient *p2p.Client) *Bot {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.FixedZone("Asia/Shanghai", 8*3600)
	}
	bot := &Bot{
		cfg:              cfg,
		store:            store,
		tg:               tg,
		tron:             tronClient,
		p2p:              p2pClient,
		loc:              loc,
		dispatcher:       worker.NewDispatcher(),
		ledgerPool:       worker.NewPool("ledger", cfg.LedgerWorkers, cfg.QueueSize),
		controlPool:      worker.NewPool("control", cfg.ControlWorkers, cfg.QueueSize),
		chainPool:        worker.NewPool("chain", cfg.ChainWorkers, cfg.QueueSize),
		ratePool:         worker.NewPool("rate", cfg.RateWorkers, cfg.QueueSize),
		broadcastPool:    worker.NewPool("broadcast", cfg.BroadcastWorkers, cfg.QueueSize),
		queryPool:        worker.NewPool("query", cfg.QueryWorkers, cfg.QueueSize),
		notifyPool:       worker.NewPool("notify", cfg.NotifyWorkers, cfg.QueueSize),
		groupTouchCache:  newTTLCache[string](cfg.GroupCacheTTL),
		userTouchCache:   newTTLCache[string](cfg.UserTouchCacheTTL),
		operatorCache:    newTTLCache[bool](cfg.OperatorCacheTTL),
		watchTargetCache: newTTLCache[[]storage.WatchTarget](cfg.WatchCacheTTL),
		rateBookCache:    newTTLCache[[]p2p.OrderBookEntry](cfg.P2PCacheTTL),
		privateStates:    newTTLCache[privateState](30 * time.Minute),
		notificationWake: make(chan struct{}, 1),
		telegramLimiter:  newTelegramRateLimiter(),
	}
	bot.textGateway = newTelegramTextGateway(tg, bot.telegramLimiter, cfg.NotifyWorkers, cfg.QueueSize)
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
	b.textGateway.Start(ctx)

	go b.addressWatchScheduler(ctx)
	go b.notificationOutboxScheduler(ctx)
	go b.rateScheduler(ctx)

	var offset int64
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
		for _, update := range updates {
			if update.UpdateID >= offset {
				offset = update.UpdateID + 1
			}
			now := time.Now().In(b.loc)
			claimed, err := b.store.ClaimUpdate(ctx, update.UpdateID, now)
			if err != nil {
				log.Printf("claim update %d: %v", update.UpdateID, err)
				continue
			}
			if !claimed {
				continue
			}
			key, pool := b.updateRoute(update)
			u := update
			b.dispatcher.Submit(ctx, key, pool, func(jobCtx context.Context) {
				start := time.Now()
				if err := b.handleUpdate(jobCtx, u); err != nil {
					log.Printf("handle update %d: %v", u.UpdateID, err)
				}
				if elapsed := time.Since(start); elapsed > 2*time.Second {
					log.Printf("slow update %d handled in %s", u.UpdateID, elapsed)
				}
			})
		}
	}
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
		return "chat:" + strconv.FormatInt(update.Message.Chat.ID, 10), b.ledgerPool
	}
	if update.CallbackQuery != nil {
		if update.CallbackQuery.Message != nil && update.CallbackQuery.Message.Chat.Type == "private" {
			return "private:" + strconv.FormatInt(update.CallbackQuery.From.ID, 10), b.controlPool
		}
		if update.CallbackQuery.Message != nil {
			return "chat:" + strconv.FormatInt(update.CallbackQuery.Message.Chat.ID, 10), b.ledgerPool
		}
		return "private:" + strconv.FormatInt(update.CallbackQuery.From.ID, 10), b.controlPool
	}
	if update.MyChatMember != nil {
		return "chat:" + strconv.FormatInt(update.MyChatMember.Chat.ID, 10), b.ledgerPool
	}
	return "update:" + strconv.FormatInt(update.UpdateID, 10), b.controlPool
}

func (b *Bot) handleUpdate(ctx context.Context, update telegram.Update) error {
	switch {
	case update.Message != nil:
		return b.handleMessage(ctx, *update.Message)
	case update.CallbackQuery != nil:
		return b.handleCallback(ctx, *update.CallbackQuery)
	case update.MyChatMember != nil:
		return b.handleMyChatMember(ctx, *update.MyChatMember)
	default:
		return nil
	}
}

func (b *Bot) handleMessage(ctx context.Context, msg telegram.Message) error {
	if msg.From == nil {
		return nil
	}
	user := userFromTelegram(*msg.From)
	now := time.Now().In(b.loc)
	text := strings.TrimSpace(msg.TextOrCaption())
	if msg.Chat.Type == "private" {
		return b.handlePrivateMessage(ctx, msg, user, text, now)
	}
	if msg.Chat.Type != "group" && msg.Chat.Type != "supergroup" {
		return nil
	}
	if err := b.ensureGroupCached(ctx, msg.Chat.ID, msg.Chat.Title, now); err != nil {
		return err
	}
	if err := b.touchUserCached(ctx, msg.Chat.ID, user, now); err != nil {
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
			return b.enqueueReplyText(ctx, sendPriorityNormal, "calc_result", msg.Chat.ID, msg.MessageID, strings.TrimSpace(text)+"="+formatCalculationResult(result), nil, now)
		}
		return nil
	}
	return b.handleLedger(ctx, msg, user, cmd, now)
}

func (b *Bot) handlePrivateMessage(ctx context.Context, msg telegram.Message, user storage.User, text string, now time.Time) error {
	if err := b.store.TouchUser(ctx, msg.Chat.ID, user, now); err != nil {
		return err
	}
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
		if !b.canUseAddressWatch(ctx, user.ID) {
			return b.enqueueReplyText(ctx, sendPriorityNormal, "private_watch_denied", msg.Chat.ID, msg.MessageID, addressWatchDeniedText, nil, now)
		}
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
	if strings.HasPrefix(cb.Data, "watch:") && !b.canUseAddressWatch(ctx, cb.From.ID) {
		return b.tg.AnswerCallback(ctx, cb.ID, addressWatchDeniedText)
	}
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
	now := time.Now().In(b.loc)
	if upd.Chat.Type != "group" && upd.Chat.Type != "supergroup" {
		return nil
	}
	switch upd.NewChatMember.Status {
	case "member", "administrator", "restricted":
		if !b.canInvite(upd.From.ID) {
			_, _ = b.tg.SendMessage(ctx, upd.Chat.ID, "邀请人没有授权，机器人将自动退出。", nil)
			return b.tg.LeaveChat(ctx, upd.Chat.ID)
		}
		if err := b.store.EnsureGroup(ctx, upd.Chat.ID, upd.Chat.Title, now); err != nil {
			return err
		}
		return nil
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
	key := strconv.FormatInt(chatID, 10)
	if cached, ok := b.groupTouchCache.Get(key); ok && cached == title {
		return nil
	}
	if err := b.store.EnsureGroup(ctx, chatID, title, now); err != nil {
		return err
	}
	b.groupTouchCache.Set(key, title)
	return nil
}

func (b *Bot) touchUserCached(ctx context.Context, chatID int64, user storage.User, now time.Time) error {
	key := fmt.Sprintf("%d:%d", chatID, user.ID)
	value := user.Username + "|" + user.DisplayName
	if cached, ok := b.userTouchCache.Get(key); ok && cached == value {
		return nil
	}
	if err := b.store.TouchUser(ctx, chatID, user, now); err != nil {
		return err
	}
	b.userTouchCache.Set(key, value)
	return nil
}

func (b *Bot) canInvite(userID int64) bool {
	return b.isRoot(userID)
}

func (b *Bot) isRoot(userID int64) bool {
	if b.cfg.HostUserID != 0 && userID == b.cfg.HostUserID {
		return true
	}
	_, ok := b.cfg.DefaultOperatorIDs[userID]
	return ok
}
