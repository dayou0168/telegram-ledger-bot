package worker

import (
	"context"
	"log"
	"sync"
)

type Executor interface {
	SubmitBlocking(context.Context, Job) bool
}

type Dispatcher struct {
	mu     sync.Mutex
	queues map[string][]queuedJob
	active map[string]bool
}

type queuedJob struct {
	executor Executor
	job      Job
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{
		queues: make(map[string][]queuedJob),
		active: make(map[string]bool),
	}
}

func (d *Dispatcher) Submit(ctx context.Context, key string, executor Executor, job Job) {
	if key == "" {
		key = "global"
	}
	var start bool
	d.mu.Lock()
	d.queues[key] = append(d.queues[key], queuedJob{executor: executor, job: job})
	if !d.active[key] {
		d.active[key] = true
		start = true
	}
	d.mu.Unlock()
	if start {
		if ok := executor.SubmitBlocking(ctx, func(runCtx context.Context) {
			d.run(runCtx, key)
		}); !ok {
			log.Printf("dispatcher submit canceled for key=%s", key)
		}
	}
}

func (d *Dispatcher) run(ctx context.Context, key string) {
	for {
		item, ok := d.pop(key)
		if !ok {
			return
		}
		item.job(ctx)
	}
}

func (d *Dispatcher) pop(key string) (queuedJob, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	queue := d.queues[key]
	if len(queue) == 0 {
		delete(d.queues, key)
		delete(d.active, key)
		return queuedJob{}, false
	}
	item := queue[0]
	copy(queue, queue[1:])
	queue = queue[:len(queue)-1]
	d.queues[key] = queue
	return item, true
}
