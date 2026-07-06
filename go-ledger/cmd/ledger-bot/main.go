package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/adminweb"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/bot"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/config"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/p2p"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/storage"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/telegram"
	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	db, err := storage.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer db.Close()

	tg := telegram.NewClient(cfg.TelegramAPIBase, cfg.TelegramBotToken, cfg.RequestTimeout)
	tronClient := tron.NewClient(cfg.TronAPIBase, cfg.TronAPIKey, cfg.RequestTimeout)
	p2pClient := p2p.NewClient(cfg.P2PAPIBase, cfg.P2PFrontAPI, cfg.RequestTimeout)

	app := bot.New(cfg, db, tg, tronClient, p2pClient)
	if cfg.AdminWebEnabled {
		web := adminweb.New(cfg, db)
		go func() {
			if err := web.Run(ctx); err != nil && err != context.Canceled {
				log.Printf("admin web stopped: %v", err)
			}
		}()
	}

	log.Printf("ledger bot go runtime v%s is starting", config.Version)
	if err := app.Run(ctx); err != nil && err != context.Canceled {
		log.Fatalf("bot stopped: %v", err)
	}
}
