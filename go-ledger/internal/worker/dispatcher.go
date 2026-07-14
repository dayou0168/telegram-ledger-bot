package worker

import (
	"context"
	"log"
	"sync"
	"sync/atomic"
)

type Executor interface {
	Submit(Job) bool
	SubmitBlocking(context.Context, Job) bool
}

type Dispatcher struct {
	mu     sync.Mutex
	queues map[string][]queuedJob
	active map[string]bool
	slots  chan struct{}
	ready  chan struct{}

	rejected atomic.Uint64
}

type queuedJob struct {
	executor Executor
	job      Job
}

type DispatcherStats struct {
	Queued     int
	ActiveKeys int
	Capacity   int
	Rejected   uint64
}

func NewDispatcher(maxQueued ...int) *Dispatcher {
	d := &Dispatcher{
		queues: make(map[string][]queuedJob),
		active: make(map[string]bool),
		ready:  make(chan struct{}, 1),
	}
	if len(maxQueued) > 0 && maxQueued[0] > 0 {
		d.slots = make(chan struct{}, maxQueued[0])
	}
	return d
}

func (d *Dispatcher) Submit(ctx context.Context, key string, executor Executor, job Job) bool {
	if key == "" {
		key = "global"
	}
	if d.slots != nil {
		select {
		case d.slots <- struct{}{}:
		case <-ctx.Done():
			return false
		}
		if ctx.Err() != nil {
			<-d.slots
			d.signalReady()
			return false
		}
	} else if ctx.Err() != nil {
		return false
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
			return false
		}
	}
	return true
}

func (d *Dispatcher) TrySubmit(ctx context.Context, key string, executor Executor, job Job) bool {
	if key == "" {
		key = "global"
	}
	if ctx.Err() != nil {
		return false
	}
	if d.slots != nil {
		select {
		case d.slots <- struct{}{}:
		default:
			d.rejected.Add(1)
			return false
		}
	}

	d.mu.Lock()
	if ctx.Err() != nil {
		d.mu.Unlock()
		d.releaseSlot()
		return false
	}
	d.queues[key] = append(d.queues[key], queuedJob{executor: executor, job: job})
	if d.active[key] {
		d.mu.Unlock()
		return true
	}
	if !executor.Submit(func(runCtx context.Context) {
		d.run(runCtx, key)
	}) {
		delete(d.queues, key)
		d.mu.Unlock()
		d.releaseSlot()
		d.rejected.Add(1)
		return false
	}
	d.active[key] = true
	d.mu.Unlock()
	return true
}

func (d *Dispatcher) Depth(key string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.queues[key])
}

func (d *Dispatcher) Stats() DispatcherStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	stats := DispatcherStats{
		ActiveKeys: len(d.active),
		Rejected:   d.rejected.Load(),
	}
	if d.slots != nil {
		stats.Capacity = cap(d.slots)
	}
	for _, queue := range d.queues {
		stats.Queued += len(queue)
	}
	return stats
}

func (d *Dispatcher) Ready() <-chan struct{} {
	return d.ready
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
	if queued > 0 {
		d.signalReady()
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
	if d.slots != nil {
		<-d.slots
	}
	d.signalReady()
	return item, true
}

func (d *Dispatcher) releaseSlot() {
	if d.slots != nil {
		<-d.slots
	}
	d.signalReady()
}

func (d *Dispatcher) signalReady() {
	select {
	case d.ready <- struct{}{}:
	default:
	}
}
