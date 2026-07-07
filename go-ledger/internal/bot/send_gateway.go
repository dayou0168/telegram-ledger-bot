package bot

import (
	"context"
	"errors"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
)

type sendPriority int

const (
	sendPriorityHigh sendPriority = iota
	sendPriorityNormal
	sendPriorityLow
)

type textSendResult struct {
	message telegram.Message
	err     error
}

type textSendRequest struct {
	chatID int64
	text   string
	opts   map[string]any
	result chan textSendResult
}

type telegramTextGateway struct {
	tg      *telegram.Client
	limiter *telegramRateLimiter
	high    chan textSendRequest
	normal  chan textSendRequest
	low     chan textSendRequest
	workers int
}

func newTelegramTextGateway(tg *telegram.Client, limiter *telegramRateLimiter, workers int, queueSize int) *telegramTextGateway {
	if workers < 1 {
		workers = 1
	}
	if queueSize < workers {
		queueSize = workers * 16
	}
	return &telegramTextGateway{
		tg:      tg,
		limiter: limiter,
		high:    make(chan textSendRequest, queueSize),
		normal:  make(chan textSendRequest, queueSize),
		low:     make(chan textSendRequest, queueSize),
		workers: workers,
	}
}

func (g *telegramTextGateway) Start(ctx context.Context) {
	if g == nil {
		return
	}
	for i := 0; i < g.workers; i++ {
		go g.loop(ctx)
	}
}

func (g *telegramTextGateway) Send(ctx context.Context, priority sendPriority, chatID int64, text string, opts map[string]any) (telegram.Message, error) {
	if g == nil || g.tg == nil {
		return telegram.Message{}, errors.New("telegram text gateway is not configured")
	}
	req := textSendRequest{
		chatID: chatID,
		text:   text,
		opts:   cloneSendOptions(opts),
		result: make(chan textSendResult, 1),
	}
	queue := g.queue(priority)
	select {
	case queue <- req:
	case <-ctx.Done():
		return telegram.Message{}, ctx.Err()
	}
	select {
	case result := <-req.result:
		return result.message, result.err
	case <-ctx.Done():
		return telegram.Message{}, ctx.Err()
	}
}

func (g *telegramTextGateway) queue(priority sendPriority) chan textSendRequest {
	switch priority {
	case sendPriorityHigh:
		return g.high
	case sendPriorityLow:
		return g.low
	default:
		return g.normal
	}
}

func (g *telegramTextGateway) loop(ctx context.Context) {
	for {
		req, ok := g.next(ctx)
		if !ok {
			return
		}
		if g.limiter != nil {
			if err := g.limiter.Wait(ctx, req.chatID); err != nil {
				req.result <- textSendResult{err: err}
				continue
			}
		}
		message, err := g.tg.SendMessage(ctx, req.chatID, req.text, req.opts)
		req.result <- textSendResult{message: message, err: err}
	}
}

func (g *telegramTextGateway) next(ctx context.Context) (textSendRequest, bool) {
	select {
	case req := <-g.high:
		return req, true
	default:
	}
	select {
	case req := <-g.high:
		return req, true
	case req := <-g.normal:
		return req, true
	default:
	}
	select {
	case req := <-g.high:
		return req, true
	case req := <-g.normal:
		return req, true
	case req := <-g.low:
		return req, true
	case <-ctx.Done():
		return textSendRequest{}, false
	}
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
	if b.textGateway != nil {
		return b.textGateway.Send(ctx, priority, chatID, text, opts)
	}
	if b.telegramLimiter != nil {
		if err := b.telegramLimiter.Wait(ctx, chatID); err != nil {
			return telegram.Message{}, err
		}
	}
	return b.tg.SendMessage(ctx, chatID, text, opts)
}
