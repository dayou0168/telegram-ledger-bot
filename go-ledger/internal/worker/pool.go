package worker

import (
	"context"
	"log"
	"sync"
	"time"
)

type Job func(context.Context)

type Pool struct {
	name        string
	jobs        chan Job
	wg          sync.WaitGroup
	mu          sync.Mutex
	ctx         context.Context
	minWorkers  int
	maxWorkers  int
	workers     int
	idleTimeout time.Duration
}

func NewPool(name string, workers int, queueSize int) *Pool {
	if workers < 1 {
		workers = 1
	}
	if queueSize < workers {
		queueSize = workers * 16
	}
	return &Pool{
		name:        name,
		jobs:        make(chan Job, queueSize),
		minWorkers:  1,
		maxWorkers:  workers,
		idleTimeout: 30 * time.Second,
	}
}

func (p *Pool) Start(ctx context.Context) {
	p.StartN(ctx, p.maxWorkers)
}

func (p *Pool) StartN(ctx context.Context, workers int) {
	if workers < 1 {
		workers = 1
	}
	p.mu.Lock()
	p.ctx = ctx
	p.maxWorkers = workers
	p.minWorkers = minWorkerCount(workers)
	for p.workers < p.minWorkers {
		p.startWorkerLocked()
	}
	p.mu.Unlock()
}

func (p *Pool) Submit(job Job) bool {
	p.maybeScale()
	select {
	case p.jobs <- job:
		p.maybeScale()
		return true
	default:
		p.maybeScale()
		return false
	}
}

func (p *Pool) SubmitBlocking(ctx context.Context, job Job) bool {
	if ctx.Err() != nil {
		return false
	}
	p.maybeScale()
	select {
	case p.jobs <- job:
		p.maybeScale()
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *Pool) Wait() {
	p.wg.Wait()
}

func (p *Pool) maybeScale() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx == nil || p.workers >= p.maxWorkers {
		return
	}
	if len(p.jobs) >= p.workers {
		p.startWorkerLocked()
	}
}

func (p *Pool) startWorkerLocked() {
	p.workers++
	p.wg.Add(1)
	go p.loop(p.ctx)
}

func (p *Pool) loop(ctx context.Context) {
	defer p.wg.Done()
	timer := time.NewTimer(p.idleTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			p.decWorker()
			return
		case job := <-p.jobs:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("worker %s panic: %v", p.name, r)
					}
				}()
				job(ctx)
			}()
			timer.Reset(p.idleTimeout)
		case <-timer.C:
			if p.canShrink() {
				p.decWorker()
				return
			}
			timer.Reset(p.idleTimeout)
		}
	}
}

func (p *Pool) canShrink() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.workers > p.minWorkers
}

func (p *Pool) decWorker() {
	p.mu.Lock()
	if p.workers > 0 {
		p.workers--
	}
	p.mu.Unlock()
}

func minWorkerCount(maxWorkers int) int {
	return 1
}
