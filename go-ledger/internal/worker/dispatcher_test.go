package worker

import (
	"context"
	"testing"
	"time"
)

func TestDispatcherBoundedQueueAppliesCancelableBackpressure(t *testing.T) {
	dispatcher := NewDispatcher(1)
	pool := NewPool("dispatcher-backpressure", 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatcher.Submit(ctx, "blocked", pool, func(context.Context) {})
	returned := make(chan struct{})
	go func() {
		dispatcher.Submit(ctx, "second", pool, func(context.Context) {})
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("second submit bypassed the dispatcher queue bound")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("backpressured submit did not unblock on cancellation")
	}
}

func TestDispatcherReleasesBoundedSlotWhenJobIsPopped(t *testing.T) {
	dispatcher := NewDispatcher(1)
	pool := NewPool("dispatcher-release", 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Start(ctx)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	dispatcher.Submit(ctx, "first", pool, func(context.Context) {
		close(firstStarted)
		<-releaseFirst
	})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first dispatcher job did not start")
	}

	secondStarted := make(chan struct{})
	dispatcher.Submit(ctx, "second", pool, func(context.Context) { close(secondStarted) })
	close(releaseFirst)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not release its bounded slot")
	}
}

func TestDispatcherCancellationDropsQueuedJobsAndStopsRunner(t *testing.T) {
	dispatcher := NewDispatcher(8)
	pool := NewPool("dispatcher-stop", 1, 8)
	ctx, cancel := context.WithCancel(context.Background())
	pool.Start(ctx)
	started := make(chan struct{})
	dispatcher.Submit(ctx, "chat", pool, func(jobCtx context.Context) {
		close(started)
		<-jobCtx.Done()
	})
	for i := 0; i < 4; i++ {
		dispatcher.Submit(ctx, "chat", pool, func(context.Context) {
			t.Error("queued job ran after cancellation")
		})
	}
	<-started
	cancel()
	done := make(chan struct{})
	go func() {
		pool.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher runner did not stop after cancellation")
	}
	if depth := dispatcher.Depth("chat"); depth != 0 {
		t.Fatalf("dispatcher retained %d canceled jobs", depth)
	}
}

func TestDispatcherTrySubmitReportsCapacityAndSignalsRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher := NewDispatcher(1)
	pool := NewPool("dispatcher-try", 1, 1)
	pool.Start(ctx)
	started := make(chan struct{})
	release := make(chan struct{})
	if !dispatcher.TrySubmit(ctx, "running", pool, func(context.Context) {
		close(started)
		<-release
	}) {
		t.Fatal("try submit running job")
	}
	<-started
	if !dispatcher.TrySubmit(ctx, "queued", pool, func(context.Context) {}) {
		t.Fatal("try submit queued job")
	}
	if dispatcher.TrySubmit(ctx, "rejected", pool, func(context.Context) {}) {
		t.Fatal("try submit exceeded dispatcher capacity")
	}
	stats := dispatcher.Stats()
	if stats.Capacity != 1 || stats.Queued != 1 || stats.Rejected == 0 {
		t.Fatalf("dispatcher stats = %+v", stats)
	}
	close(release)
	select {
	case <-dispatcher.Ready():
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not signal released capacity")
	}
}
