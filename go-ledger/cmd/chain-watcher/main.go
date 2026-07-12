package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/chainwatcher"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadChainWatcher()
	if err != nil {
		log.Fatalf("load chain watcher config: %v", err)
	}
	db, err := storage.OpenChainWatcher(ctx, cfg.DatabaseURL, cfg.KeyEncryptionKey)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer db.Close()

	tronClient := tron.NewClientWithKeys(cfg.TronAPIBase, cfg.TronAPIKeys, cfg.RequestTimeout, tron.KeyPoolOptions{
		MinInterval:          cfg.RequestInterval,
		AuthProbeInterval:    cfg.KeyAuthProbeInterval,
		InvalidProbeInterval: cfg.KeyInvalidProbeInterval,
		BlockedProbeInterval: cfg.KeyBlockedProbeInterval,
		CompensationMaxRPS:   cfg.CatchupMaxRPS,
		BudgetZone:           cfg.BudgetLocation,
		UsageStore:           db,
	})
	tronClient.ConfigureMainBudget(cfg.GlobalPages, cfg.PollInterval)
	if err := tronClient.SeedAndRefreshKeyRegistry(ctx, cfg.TronAPIKeys); err != nil {
		log.Fatalf("seed tronscan key registry: %v", err)
	}
	if err := tronClient.RestoreKeyPool(ctx); err != nil {
		log.Fatalf("restore tronscan key usage: %v", err)
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := tronClient.RefreshKeyRegistry(ctx); err != nil {
					log.Printf("refresh tronscan key registry: %v", err)
				}
			}
		}
	}()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				probeCtx, cancel := context.WithTimeout(ctx, cfg.RequestTimeout)
				tronClient.ProbeDueKeys(probeCtx, cfg.USDTContract)
				cancel()
			}
		}
	}()
	if keyStatus := tronClient.KeyPoolStatus(time.Now()); !keyStatus.MainCapacitySafe {
		log.Printf("WARNING: tronscan main-scan capacity is unsafe: %s", keyStatus.CapacityWarning)
	}
	app := chainwatcher.NewServer(cfg, db, tronClient)
	log.Printf("ledger chain watcher v%s is starting", config.Version)
	if err := app.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("chain watcher stopped: %v", err)
	}
}
