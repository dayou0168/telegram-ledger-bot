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
