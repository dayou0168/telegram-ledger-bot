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
	slots  chan struct{}
}

type queuedJob struct {
	executor Executor
	job      Job
}

func NewDispatcher(maxQueued ...int) *Dispatcher {
	d := &Dispatcher{
		queues: make(map[string][]queuedJob),
		active: make(map[string]bool),
	}
	if len(maxQueued) > 0 && maxQueued[0] > 0 {
		d.slots = make(chan struct{}, maxQueued[0])
	}
	return d
}

func (d *Dispatcher) Submit(ctx context.Context, key string, executor Executor, job Job) {
	if key == "" {
		key = "global"
	}
	if d.slots != nil {
		select {
		case d.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		if ctx.Err() != nil {
			<-d.slots
			return
		}
	} else if ctx.Err() != nil {
		return
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
			d.discard(key)
			log.Printf("dispatcher submit canceled for key=%s", key)
		}
	}
}

func (d *Dispatcher) Depth(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queues[key])
}

func (d *Dispatcher) run(ctx context.Context, key string) {
	for {
		if ctx.Err() != nil {
			d.discard(key)
			return
		}
		item, ok := d.pop(key)
		if !ok {
			return
		}
		item.job(ctx)
	}
}

func (d *Dispatcher) discard(key string) {
	d.mu.Lock()
	queued := len(d.queues[key])
	delete(d.queues, key)
	delete(d.active, key)
	if d.slots != nil {
		for i := 0; i < queued; i++ {
			<-d.slots
		}
	}
	d.mu.Unlock()
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
	if d.slots != nil {
		<-d.slots
	}
	return item, true
}
