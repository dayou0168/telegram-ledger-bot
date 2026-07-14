package bot

import (
	"context"
	"encoding/json"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/worker"
)

func TestDurableTelegramPayloadLegacyPrivateStateRoundTrip(t *testing.T) {
	want := privateState{
		Mode: "quick_reply", TargetName: "source", ChatIDs: []int64{-101, -102}, NotifyAll: true,
		ControlMessageID: 41, WatchAddress: "TSource", QuickReplyTargetChat: -202,
		QuickReplyMessageID: 42, ReturnMode: "group", ReturnTargetName: "saved",
		ReturnChatIDs: []int64{-101, -102}, ReturnNotifyAll: true, ReturnControlMessageID: 43,
		CreatedAt: time.Date(2026, 7, 15, 10, 11, 12, 0, time.UTC),
	}
	payload, err := json.Marshal(durableTelegramPayload{Version: 1, Update: privateMessageUpdate(801, 901, "legacy"), LegacyPrivateState: &want})
	if err != nil {
		t.Fatal(err)
	}
	var decoded durableTelegramPayload
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.LegacyPrivateState == nil || !reflect.DeepEqual(*decoded.LegacyPrivateState, want) {
		t.Fatalf("legacy private state round trip=%+v want=%+v", decoded.LegacyPrivateState, want)
	}
}

func TestUpdateAdmissionBypassFloodDoesNotBlockLedgerAndLosesNoJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ledgerPool := worker.NewPool("admission-ledger", 1, 2)
	bypassPool := worker.NewPool("admission-bypass", 1, 1)
	ledgerPool.Start(ctx)
	bypassPool.Start(ctx)
	admission := newUpdateAdmission(2)
	admission.Start(ctx)

	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	done := make(chan struct{}, 4)
	if !admission.Submit(ctx, updateAdmissionBypass, updateAdmissionJob{
		key: "bypass:first", executor: bypassPool,
		job: func(jobCtx context.Context) {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-jobCtx.Done():
			}
			done <- struct{}{}
		},
	}) {
		t.Fatal("submit first bypass job")
	}
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first bypass job did not start")
	}

	for i := 0; i < 3; i++ {
		value := i
		if !admission.Submit(ctx, updateAdmissionBypass, updateAdmissionJob{
			key: "bypass:" + string(rune('a'+value)), executor: bypassPool,
			job: func(context.Context) { done <- struct{}{} },
		}) {
			t.Fatalf("submit bypass overflow job %d", i)
		}
	}
	stats := admission.Stats()
	if stats.BypassOverflow == 0 || stats.BypassOverflow > stats.OverflowCapacity {
		t.Fatalf("bypass overflow stats = %+v", stats)
	}

	ledgerDone := make(chan struct{})
	if !admission.Submit(ctx, updateAdmissionLedger, updateAdmissionJob{
		key: "ledger:-1001", executor: ledgerPool,
		job: func(context.Context) { close(ledgerDone) },
	}) {
		t.Fatal("submit ledger during bypass flood")
	}
	select {
	case <-ledgerDone:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("bypass flood blocked ledger admission")
	}

	close(releaseFirst)
	for i := 0; i < 4; i++ {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("accepted bypass job %d was lost", i)
		}
	}
	cancel()
	admission.Wait()
	ledgerPool.Wait()
	bypassPool.Wait()
}

func TestUpdateAdmissionPreservesSameChatLedgerFIFO(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := worker.NewPool("admission-ledger-fifo", 3, 8)
	pool.Start(ctx)
	admission := newUpdateAdmission(8)
	admission.Start(ctx)
	order := make(chan int, 3)
	for i := 1; i <= 3; i++ {
		value := i
		if !admission.Submit(ctx, updateAdmissionLedger, updateAdmissionJob{
			key: "ledger:-2002", executor: pool,
			job: func(context.Context) { order <- value },
		}) {
			t.Fatalf("submit ledger job %d", i)
		}
	}
	for want := 1; want <= 3; want++ {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("ledger FIFO order = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("ledger FIFO timed out")
		}
	}
	cancel()
	admission.Wait()
	pool.Wait()
}

func TestUpdateAdmissionBackpressureIsBoundedAndCancelable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	pool := worker.NewPool("admission-cancel", 1, 1)
	pool.Start(ctx)
	admission := newUpdateAdmission(1)
	admission.Start(ctx)

	started := make(chan struct{})
	if !admission.Submit(ctx, updateAdmissionBypass, updateAdmissionJob{
		key: "running", executor: pool,
		job: func(jobCtx context.Context) {
			close(started)
			<-jobCtx.Done()
		},
	}) {
		t.Fatal("submit running bypass job")
	}
	<-started
	if !admission.Submit(ctx, updateAdmissionBypass, updateAdmissionJob{key: "queued", executor: pool, job: func(context.Context) {}}) {
		t.Fatal("submit queued bypass job")
	}
	if !admission.Submit(ctx, updateAdmissionBypass, updateAdmissionJob{key: "overflow", executor: pool, job: func(context.Context) {}}) {
		t.Fatal("submit overflow bypass job")
	}

	var returned atomic.Bool
	result := make(chan bool, 1)
	go func() {
		accepted := admission.Submit(ctx, updateAdmissionBypass, updateAdmissionJob{
			key: "backpressured", executor: pool, job: func(context.Context) {},
		})
		returned.Store(true)
		result <- accepted
	}()
	select {
	case <-result:
		t.Fatal("submission bypassed full bounded admission")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case ok := <-result:
		if ok {
			t.Fatal("canceled backpressured submission was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("backpressured submission did not unblock on cancellation")
	}
	if !returned.Load() || admission.Stats().BackpressureCount == 0 {
		t.Fatalf("backpressure metrics = %+v", admission.Stats())
	}
	admission.Wait()
	pool.Wait()
}
