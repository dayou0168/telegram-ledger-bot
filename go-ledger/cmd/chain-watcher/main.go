package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

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
	db, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer db.Close()

	tronClient := tron.NewClient(cfg.TronAPIBase, cfg.TronAPIKey, cfg.RequestTimeout)
	tronClient.SetMinRequestInterval(cfg.RequestInterval)
	app := chainwatcher.NewServer(cfg, db, tronClient)
	log.Printf("ledger chain watcher v%s is starting", config.Version)
	if err := app.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("chain watcher stopped: %v", err)
	}
}
