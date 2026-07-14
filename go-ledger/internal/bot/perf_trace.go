package bot

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

type perfTraceKey struct{}

type perfTrace struct {
	mu         sync.Mutex
	updateID   int64
	chatID     int64
	command    string
	startedAt  time.Time
	stages     map[string]time.Duration
	caches     map[string]string
	queueKey   string
	queueDepth int
}

type perfLog struct {
	Kind       string            `json:"kind"`
	Update     int64             `json:"update_id"`
	ChatID     int64             `json:"chat_id"`
	Command    string            `json:"command"`
	TotalMS    int64             `json:"total_ms"`
	Stages     map[string]int64  `json:"stages_ms,omitempty"`
	Caches     map[string]string `json:"caches,omitempty"`
	QueueKey   string            `json:"queue_key,omitempty"`
	QueueDepth int               `json:"queue_depth,omitempty"`
}

func addPerfStage(ctx context.Context, stage string, duration time.Duration) {
	if trace := perfTraceFromContext(ctx); trace != nil {
		trace.mu.Lock()
		trace.stages[stage] += duration
		trace.mu.Unlock()
	}
}

func markPerfQueue(ctx context.Context, key string, depth int) {
	if trace := perfTraceFromContext(ctx); trace != nil {
		trace.mu.Lock()
		trace.queueKey = key
		trace.queueDepth = depth
		trace.mu.Unlock()
	}
}

func newPerfTrace(updateID, chatID int64) *perfTrace {
	return &perfTrace{
		updateID:  updateID,
		chatID:    chatID,
		command:   "unknown",
		startedAt: time.Now(),
		stages:    make(map[string]time.Duration),
		caches:    make(map[string]string),
	}
}

func contextWithPerfTrace(ctx context.Context, trace *perfTrace) context.Context {
	if trace == nil {
		return ctx
	}
	return context.WithValue(ctx, perfTraceKey{}, trace)
}

func perfTraceFromContext(ctx context.Context) *perfTrace {
	if trace, ok := ctx.Value(perfTraceKey{}).(*perfTrace); ok {
		return trace
	}
	return nil
}

func setPerfCommand(ctx context.Context, command string) {
	if trace := perfTraceFromContext(ctx); trace != nil {
		trace.mu.Lock()
		trace.command = command
		trace.mu.Unlock()
	}
}

func markPerfCache(ctx context.Context, name string, hit bool) {
	if trace := perfTraceFromContext(ctx); trace != nil {
		value := "miss"
		if hit {
			value = "hit"
		}
		trace.mu.Lock()
		trace.caches[name] = value
		trace.mu.Unlock()
	}
}

func measurePerfStage(ctx context.Context, stage string) func() {
	trace := perfTraceFromContext(ctx)
	if trace == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		trace.mu.Lock()
		trace.stages[stage] += time.Since(start)
		trace.mu.Unlock()
	}
}

func finishPerfTrace(trace *perfTrace, threshold time.Duration) {
	if trace == nil || threshold <= 0 {
		return
	}
	total := time.Since(trace.startedAt)
	if total < threshold {
		return
	}
	trace.mu.Lock()
	stages := make(map[string]int64, len(trace.stages))
	for name, duration := range trace.stages {
		stages[name] = duration.Milliseconds()
	}
	caches := make(map[string]string, len(trace.caches))
	for name, value := range trace.caches {
		caches[name] = value
	}
	command := trace.command
	queueKey := trace.queueKey
	queueDepth := trace.queueDepth
	trace.mu.Unlock()
	payload := perfLog{
		Kind:       "slow_update",
		Update:     trace.updateID,
		ChatID:     trace.chatID,
		Command:    command,
		TotalMS:    total.Milliseconds(),
		Stages:     stages,
		Caches:     caches,
		QueueKey:   queueKey,
		QueueDepth: queueDepth,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("slow_update update_id=%d chat_id=%d command=%s total_ms=%d", trace.updateID, trace.chatID, command, total.Milliseconds())
		return
	}
	log.Printf("%s", raw)
}
