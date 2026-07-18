package bot

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type sendPriority int

const (
	sendPriorityCritical sendPriority = iota
	sendPriorityNormal
	sendPriorityBulk

	sendPriorityHigh = sendPriorityCritical
	sendPriorityLow  = sendPriorityBulk
)

var errTelegramSendGatewayNotConfigured = errors.New("telegram send gateway is not configured")

func (p sendPriority) normalized() sendPriority {
	switch p {
	case sendPriorityCritical, sendPriorityNormal, sendPriorityBulk:
		return p
	default:
		return sendPriorityNormal
	}
}

func (p sendPriority) String() string {
	switch p.normalized() {
	case sendPriorityCritical:
		return "critical"
	case sendPriorityBulk:
		return "bulk"
	default:
		return "normal"
	}
}

type telegramSendResult struct {
	message telegram.Message
	err     error
	metrics telegramSendMetrics
}

type telegramSendOperation func(context.Context) (telegram.Message, error)

type telegramSendMetrics struct {
	EnqueuedAt         time.Time
	StartedAt          time.Time
	CompletedAt        time.Time
	QueueWaitDuration  time.Duration
	RateLimitDuration  time.Duration
	OperationDuration  time.Duration
	RetryDelayDuration time.Duration
	TotalDuration      time.Duration
	Attempts           int
	Priority           sendPriority
}

type TelegramSendGatewayStats struct {
	Workers     int                          `json:"workers"`
	MaxAttempts int                          `json:"max_attempts"`
	Critical    TelegramSendGatewayLaneStats `json:"critical"`
	Normal      TelegramSendGatewayLaneStats `json:"normal"`
	Bulk        TelegramSendGatewayLaneStats `json:"bulk"`
}

type TelegramSendGatewayLaneStats struct {
	Queued            int        `json:"queued"`
	Capacity          int        `json:"capacity"`
	OldestQueuedAt    *time.Time `json:"oldest_queued_at,omitempty"`
	OldestQueuedAgeMS int64      `json:"oldest_queued_age_ms"`
}

type telegramSendRequest struct {
	ctx         context.Context
	chatID      int64
	operation   telegramSendOperation
	result      chan telegramSendResult
	maxAttempts int
	enqueuedAt  time.Time
	priority    sendPriority
}

type telegramSendGateway struct {
	tg      *telegram.Client
	limiter *telegramRateLimiter
	workers int

	critical chan telegramSendRequest
	normal   chan telegramSendRequest
	bulk     chan telegramSendRequest

	maxAttempts    int
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration

	metricsMu sync.Mutex
	queuedAt  map[sendPriority][]time.Time
}

func newTelegramSendGateway(tg *telegram.Client, limiter *telegramRateLimiter, workers int, queueSize int) *telegramSendGateway {
	if workers < 3 {
		workers = 3
	}
	if queueSize < workers {
		queueSize = workers * 16
	}
	return &telegramSendGateway{
		tg:             tg,
		limiter:        limiter,
		workers:        workers,
		critical:       make(chan telegramSendRequest, queueSize),
		normal:         make(chan telegramSendRequest, queueSize),
		bulk:           make(chan telegramSendRequest, queueSize),
		maxAttempts:    3,
		retryBaseDelay: 200 * time.Millisecond,
		retryMaxDelay:  2 * time.Second,
		queuedAt: map[sendPriority][]time.Time{
			sendPriorityCritical: {},
			sendPriorityNormal:   {},
			sendPriorityBulk:     {},
		},
	}
}

func (g *telegramSendGateway) Start(ctx context.Context) {
	if g == nil {
		return
	}
	criticalWorkers, normalWorkers, bulkWorkers := splitGatewayWorkers(g.workers)
	for i := 0; i < criticalWorkers; i++ {
		go g.loop(ctx, sendPriorityCritical)
	}
	for i := 0; i < normalWorkers; i++ {
		go g.loop(ctx, sendPriorityNormal)
	}
	for i := 0; i < bulkWorkers; i++ {
		go g.loop(ctx, sendPriorityBulk)
	}
}

func splitGatewayWorkers(workers int) (int, int, int) {
	if workers < 3 {
		workers = 3
	}
	critical := workers / 3
	if critical < 1 {
		critical = 1
	}
	normal := workers / 3
	if normal < 1 {
		normal = 1
	}
	bulk := workers - critical - normal
	if bulk < 1 {
		bulk = 1
	}
	return critical, normal, bulk
}

func (g *telegramSendGateway) Do(ctx context.Context, priority sendPriority, chatID int64, operation telegramSendOperation) (telegram.Message, error) {
	message, err, _ := g.DoWithMetrics(ctx, priority, chatID, operation)
	return message, err
}

func (g *telegramSendGateway) DoOnce(ctx context.Context, priority sendPriority, chatID int64, operation telegramSendOperation) (telegram.Message, error) {
	message, err, _ := g.doWithMetrics(ctx, priority, chatID, 1, operation)
	return message, err
}

func (g *telegramSendGateway) DoWithMetrics(ctx context.Context, priority sendPriority, chatID int64, operation telegramSendOperation) (telegram.Message, error, telegramSendMetrics) {
	maxAttempts := 1
	if g != nil {
		maxAttempts = g.maxAttempts
	}
	return g.doWithMetrics(ctx, priority, chatID, maxAttempts, operation)
}

func (g *telegramSendGateway) doWithMetrics(ctx context.Context, priority sendPriority, chatID int64, maxAttempts int, operation telegramSendOperation) (telegram.Message, error, telegramSendMetrics) {
	if g == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured, telegramSendMetrics{}
	}
	if operation == nil {
		return telegram.Message{}, errors.New("telegram send operation is nil"), telegramSendMetrics{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	priority = priority.normalized()
	req := telegramSendRequest{
		ctx:         ctx,
		chatID:      chatID,
		operation:   operation,
		result:      make(chan telegramSendResult, 1),
		maxAttempts: maxAttempts,
		enqueuedAt:  time.Now(),
		priority:    priority,
	}
	g.noteQueued(priority, req.enqueuedAt)
	queue := g.queue(priority)
	select {
	case queue <- req:
	case <-ctx.Done():
		g.noteDequeued(priority)
		return telegram.Message{}, ctx.Err(), telegramSendMetrics{}
	}
	select {
	case result := <-req.result:
		return result.message, result.err, result.metrics
	case <-ctx.Done():
		return telegram.Message{}, ctx.Err(), telegramSendMetrics{}
	}
}

func (g *telegramSendGateway) SendMessageWithMetrics(ctx context.Context, priority sendPriority, chatID int64, text string, opts map[string]any) (telegram.Message, error, telegramSendMetrics) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured, telegramSendMetrics{}
	}
	cloned := cloneSendOptions(opts)
	return g.DoWithMetrics(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.SendMessage(opCtx, chatID, text, cloned)
	})
}

func (g *telegramSendGateway) SendMessage(ctx context.Context, priority sendPriority, chatID int64, text string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.SendMessage(opCtx, chatID, text, cloned)
	})
}

func (g *telegramSendGateway) CopyMessage(ctx context.Context, priority sendPriority, chatID, fromChatID, messageID int64, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.CopyMessage(opCtx, chatID, fromChatID, messageID, cloned)
	})
}

func (g *telegramSendGateway) EditMessageText(ctx context.Context, priority sendPriority, chatID, messageID int64, text string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.EditMessageText(opCtx, chatID, messageID, text, cloned)
	})
}

func (g *telegramSendGateway) EditMessageCaption(ctx context.Context, priority sendPriority, chatID, messageID int64, caption string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.EditMessageCaption(opCtx, chatID, messageID, caption, cloned)
	})
}

func (g *telegramSendGateway) SendPhotoBytes(ctx context.Context, priority sendPriority, chatID int64, filename string, data []byte, caption string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	payload := append([]byte(nil), data...)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.SendPhotoBytes(opCtx, chatID, filename, payload, caption, cloned)
	})
}

func (g *telegramSendGateway) SendPhoto(ctx context.Context, priority sendPriority, chatID int64, fileID, caption string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.SendPhoto(opCtx, chatID, fileID, caption, cloned)
	})
}

func (g *telegramSendGateway) EditMessagePhotoBytes(ctx context.Context, priority sendPriority, chatID, messageID int64, filename string, data []byte, caption string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errTelegramSendGatewayNotConfigured
	}
	cloned := cloneSendOptions(opts)
	payload := append([]byte(nil), data...)
	return g.Do(ctx, priority, chatID, func(opCtx context.Context) (telegram.Message, error) {
		return g.tg.EditMessagePhotoBytes(opCtx, chatID, messageID, filename, payload, caption, cloned)
	})
}

func (g *telegramSendGateway) queue(priority sendPriority) chan telegramSendRequest {
	switch priority.normalized() {
	case sendPriorityCritical:
		return g.critical
	case sendPriorityBulk:
		return g.bulk
	default:
		return g.normal
	}
}

func (g *telegramSendGateway) loop(ctx context.Context, lane sendPriority) {
	for {
		req, ok := g.next(ctx, lane)
		if !ok {
			return
		}
		g.noteDequeued(req.priority)
		message, err, metrics := g.execute(ctx, req)
		req.result <- telegramSendResult{message: message, err: err, metrics: metrics}
	}
}

func (g *telegramSendGateway) Stats(now time.Time) TelegramSendGatewayStats {
	if g == nil {
		return TelegramSendGatewayStats{}
	}
	if now.IsZero() {
		now = time.Now()
	}
	g.metricsMu.Lock()
	defer g.metricsMu.Unlock()
	return TelegramSendGatewayStats{
		Workers:     g.workers,
		MaxAttempts: g.maxAttempts,
		Critical:    g.laneStatsLocked(sendPriorityCritical, cap(g.critical), now),
		Normal:      g.laneStatsLocked(sendPriorityNormal, cap(g.normal), now),
		Bulk:        g.laneStatsLocked(sendPriorityBulk, cap(g.bulk), now),
	}
}

func (g *telegramSendGateway) noteQueued(priority sendPriority, queuedAt time.Time) {
	if g == nil {
		return
	}
	priority = priority.normalized()
	g.metricsMu.Lock()
	g.queuedAt[priority] = append(g.queuedAt[priority], queuedAt)
	g.metricsMu.Unlock()
}

func (g *telegramSendGateway) noteDequeued(priority sendPriority) {
	if g == nil {
		return
	}
	priority = priority.normalized()
	g.metricsMu.Lock()
	queue := g.queuedAt[priority]
	if len(queue) > 0 {
		copy(queue, queue[1:])
		g.queuedAt[priority] = queue[:len(queue)-1]
	}
	g.metricsMu.Unlock()
}

func (g *telegramSendGateway) laneStatsLocked(priority sendPriority, capacity int, now time.Time) TelegramSendGatewayLaneStats {
	queue := g.queuedAt[priority.normalized()]
	stats := TelegramSendGatewayLaneStats{
		Queued:   len(queue),
		Capacity: capacity,
	}
	if len(queue) > 0 {
		oldest := queue[0]
		stats.OldestQueuedAt = &oldest
		if age := now.Sub(oldest); age > 0 {
			stats.OldestQueuedAgeMS = age.Milliseconds()
		}
	}
	return stats
}

func (g *telegramSendGateway) next(ctx context.Context, lane sendPriority) (telegramSendRequest, bool) {
	switch lane.normalized() {
	case sendPriorityCritical:
		select {
		case req := <-g.critical:
			return req, true
		case <-ctx.Done():
			return telegramSendRequest{}, false
		}
	case sendPriorityBulk:
		select {
		case req := <-g.bulk:
			return req, true
		case <-ctx.Done():
			return telegramSendRequest{}, false
		}
	default:
		select {
		case req := <-g.critical:
			return req, true
		default:
		}
		select {
		case req := <-g.critical:
			return req, true
		case req := <-g.normal:
			return req, true
		case <-ctx.Done():
			return telegramSendRequest{}, false
		}
	}
}

func (g *telegramSendGateway) execute(workerCtx context.Context, req telegramSendRequest) (telegram.Message, error, telegramSendMetrics) {
	metrics := telegramSendMetrics{
		EnqueuedAt: req.enqueuedAt,
		StartedAt:  time.Now(),
		Priority:   req.priority,
	}
	if metrics.EnqueuedAt.IsZero() {
		metrics.EnqueuedAt = metrics.StartedAt
	}
	metrics.QueueWaitDuration = metrics.StartedAt.Sub(metrics.EnqueuedAt)
	maxAttempts := req.maxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	for attempt := 1; ; attempt++ {
		metrics.Attempts = attempt
		if err := contextDone(workerCtx, req.ctx); err != nil {
			metrics.CompletedAt = time.Now()
			metrics.TotalDuration = metrics.CompletedAt.Sub(metrics.EnqueuedAt)
			return telegram.Message{}, err, metrics
		}
		if g.limiter != nil {
			rateStarted := time.Now()
			if err := g.limiter.Wait(req.ctx, req.priority, req.chatID); err != nil {
				metrics.RateLimitDuration += time.Since(rateStarted)
				metrics.CompletedAt = time.Now()
				metrics.TotalDuration = metrics.CompletedAt.Sub(metrics.EnqueuedAt)
				return telegram.Message{}, err, metrics
			}
			metrics.RateLimitDuration += time.Since(rateStarted)
		}
		operationStarted := time.Now()
		message, err := req.operation(req.ctx)
		metrics.OperationDuration += time.Since(operationStarted)
		if err == nil {
			metrics.CompletedAt = time.Now()
			metrics.TotalDuration = metrics.CompletedAt.Sub(metrics.EnqueuedAt)
			return message, nil, metrics
		}
		if attempt >= maxAttempts || !telegramGatewayRetryable(err) {
			metrics.CompletedAt = time.Now()
			metrics.TotalDuration = metrics.CompletedAt.Sub(metrics.EnqueuedAt)
			return telegram.Message{}, err, metrics
		}
		delay := telegramGatewayRetryDelay(attempt, err, g.retryBaseDelay, g.retryMaxDelay)
		metrics.RetryDelayDuration += delay
		if err := waitGatewayDelay(workerCtx, req.ctx, delay); err != nil {
			metrics.CompletedAt = time.Now()
			metrics.TotalDuration = metrics.CompletedAt.Sub(metrics.EnqueuedAt)
			return telegram.Message{}, err, metrics
		}
	}
}

func contextDone(contexts ...context.Context) error {
	for _, ctx := range contexts {
		if ctx == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return nil
}

func waitGatewayDelay(workerCtx, reqCtx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return contextDone(workerCtx, reqCtx)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-workerCtx.Done():
		return workerCtx.Err()
	case <-reqCtx.Done():
		return reqCtx.Err()
	case <-timer.C:
		return nil
	}
}

func telegramGatewayRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, errQuickReplyPermissionRevoked) || errors.Is(err, errQuickReplyLeaseLost) {
		return false
	}
	if _, ok := telegram.RetryAfter(err); ok {
		return true
	}
	var tgErr *telegram.Error
	if errors.As(err, &tgErr) {
		return tgErr.ErrorCode == 429 || tgErr.ErrorCode >= 500
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return true
}

func telegramGatewayRetryDelay(attempt int, err error, baseDelay, maxDelay time.Duration) time.Duration {
	if retryAfter, ok := telegram.RetryAfter(err); ok {
		return retryAfter + time.Second
	}
	if baseDelay <= 0 {
		baseDelay = 200 * time.Millisecond
	}
	if maxDelay <= 0 {
		maxDelay = 2 * time.Second
	}
	delay := baseDelay
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	if delay > maxDelay {
		return maxDelay
	}
	return delay
}

func cloneSendOptions(opts map[string]any) map[string]any {
	if len(opts) == 0 {
		return nil
	}
	clone := make(map[string]any, len(opts))
	for key, value := range opts {
		clone[key] = value
	}
	return clone
}

func (b *Bot) sendText(ctx context.Context, priority sendPriority, chatID int64, text string, opts map[string]any) (telegram.Message, error) {
	done := measurePerfStage(ctx, "send_enqueue")
	defer done()
	var msg telegram.Message
	var err error
	if b.sendGateway != nil {
		msg, err = b.sendGateway.SendMessage(ctx, priority, chatID, text, opts)
	} else {
		err = errTelegramSendGatewayNotConfigured
	}
	if err == nil {
		b.recordOutgoingPrivateChatMessage(ctx, msg, "outgoing")
	}
	return msg, err
}

func (b *Bot) copyMessage(ctx context.Context, chatID, fromChatID, messageID int64, opts map[string]any) (telegram.Message, error) {
	return b.copyMessageWithPriority(ctx, sendPriorityNormal, chatID, fromChatID, messageID, opts)
}

func (b *Bot) copyMessageWithPriority(ctx context.Context, priority sendPriority, chatID, fromChatID, messageID int64, opts map[string]any) (telegram.Message, error) {
	var msg telegram.Message
	var err error
	if b.sendGateway != nil {
		msg, err = b.sendGateway.CopyMessage(ctx, priority, chatID, fromChatID, messageID, opts)
	} else {
		err = errTelegramSendGatewayNotConfigured
	}
	if err == nil {
		b.recordOutgoingPrivateChatMessage(ctx, msg, "outgoing_copy")
	}
	return msg, err
}

func (b *Bot) editText(ctx context.Context, chatID, messageID int64, text string, opts map[string]any) (telegram.Message, error) {
	if b.sendGateway != nil {
		return b.sendGateway.EditMessageText(ctx, sendPriorityNormal, chatID, messageID, text, opts)
	}
	return telegram.Message{}, errTelegramSendGatewayNotConfigured
}

func (b *Bot) editCaption(ctx context.Context, chatID, messageID int64, caption string, opts map[string]any) (telegram.Message, error) {
	if b.sendGateway != nil {
		return b.sendGateway.EditMessageCaption(ctx, sendPriorityNormal, chatID, messageID, caption, opts)
	}
	return telegram.Message{}, errTelegramSendGatewayNotConfigured
}

func (b *Bot) sendPhotoBytes(ctx context.Context, chatID int64, filename string, data []byte, caption string, opts map[string]any) (telegram.Message, error) {
	return b.sendPhotoBytesWithPriority(ctx, sendPriorityNormal, chatID, filename, data, caption, opts)
}

func (b *Bot) sendPhotoBytesWithPriority(ctx context.Context, priority sendPriority, chatID int64, filename string, data []byte, caption string, opts map[string]any) (telegram.Message, error) {
	done := measurePerfStage(ctx, "send_enqueue")
	defer done()
	var msg telegram.Message
	var err error
	if b.sendGateway != nil {
		msg, err = b.sendGateway.SendPhotoBytes(ctx, priority, chatID, filename, data, caption, opts)
	} else {
		err = errTelegramSendGatewayNotConfigured
	}
	if err == nil {
		b.recordOutgoingPrivateChatMessage(ctx, msg, "outgoing_photo")
	}
	return msg, err
}

func (b *Bot) editPhotoBytes(ctx context.Context, chatID, messageID int64, filename string, data []byte, caption string, opts map[string]any) (telegram.Message, error) {
	if b.sendGateway != nil {
		return b.sendGateway.EditMessagePhotoBytes(ctx, sendPriorityNormal, chatID, messageID, filename, data, caption, opts)
	}
	return telegram.Message{}, errTelegramSendGatewayNotConfigured
}

func (b *Bot) TelegramSendGatewayStats() any {
	if b == nil || b.sendGateway == nil {
		return nil
	}
	return b.sendGateway.Stats(time.Now())
}
