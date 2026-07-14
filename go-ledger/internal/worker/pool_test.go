package worker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestElasticPoolRunsJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool("test", 4, 16)
	pool.StartN(ctx, 4)

	var count int32
	for i := 0; i < 8; i++ {
		if !pool.SubmitBlocking(ctx, func(context.Context) {
			atomic.AddInt32(&count, 1)
		}) {
			t.Fatal("submit failed")
		}
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&count) == 8 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("count = %d", count)
}

func TestElasticPoolScalesWhenFirstWorkerIsBusy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewPool("cross-chat", 4, 16)
	pool.StartN(ctx, 4)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	if !pool.SubmitBlocking(ctx, func(context.Context) {
		close(firstStarted)
		<-releaseFirst
	}) {
		t.Fatal("submit first job")
	}
	<-firstStarted
	secondDone := make(chan struct{})
	if !pool.SubmitBlocking(ctx, func(context.Context) { close(secondDone) }) {
		t.Fatal("submit second job")
	}
	select {
	case <-secondDone:
		close(releaseFirst)
	case <-time.After(time.Second):
		close(releaseFirst)
		t.Fatal("queued job did not trigger elastic worker growth")
	}
}

func TestPoolStopsAllWorkersAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := NewPool("stop", 4, 16)
	pool.Start(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		pool.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("workers did not stop after context cancellation")
	}
}
