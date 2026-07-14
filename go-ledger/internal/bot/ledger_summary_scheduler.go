package bot

import (
	"context"
	"log"
	"time"
)

func (b *Bot) ledgerSummaryReconcileScheduler(ctx context.Context) {
	if b.cfg.LedgerSummaryWriteMode != "shadow" && b.cfg.LedgerSummaryReadMode != "safe" {
		return
	}
	interval := b.cfg.LedgerSummaryReconcileEvery
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats, err := b.store.ReconcileLedgerSummaries(ctx, 0, 200, time.Now().In(b.loc))
			if err != nil {
				log.Printf("reconcile ledger summaries: %v", err)
				continue
			}
			if stats.Corrected > 0 {
				b.clearBillSummaryCache()
				log.Printf("ledger summary reconcile: scanned=%d corrected=%d unchanged=%d", stats.Scanned, stats.Corrected, stats.Unchanged)
			}
		}
	}
}
