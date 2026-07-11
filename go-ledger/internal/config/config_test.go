package config

import "testing"

func TestLocalAddressWatcherEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want bool
	}{
		{
			name: "watcher not configured keeps local scanner on",
			cfg:  Config{},
			want: true,
		},
		{
			name: "watcher configured disables local scanner by default",
			cfg: Config{
				ChainWatcherURL:    "http://ledger-chain-watcher:8090",
				ChainWatcherBotID:  "bot-a",
				ChainWatcherSecret: "secret",
			},
			want: false,
		},
		{
			name: "emergency fallback runs local scanner with watcher",
			cfg: Config{
				ChainWatcherURL:               "http://ledger-chain-watcher:8090",
				ChainWatcherBotID:             "bot-a",
				ChainWatcherSecret:            "secret",
				ChainWatcherEmergencyFallback: true,
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.LocalAddressWatcherEnabled(); got != tt.want {
				t.Fatalf("LocalAddressWatcherEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadChainWatcherEmergencyFallbackDefaultAndEnv(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("CHAIN_WATCHER_EMERGENCY_FALLBACK", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ChainWatcherEmergencyFallback {
		t.Fatal("ChainWatcherEmergencyFallback default = true, want false")
	}

	t.Setenv("CHAIN_WATCHER_EMERGENCY_FALLBACK", "true")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.ChainWatcherEmergencyFallback {
		t.Fatal("ChainWatcherEmergencyFallback env = false, want true")
	}
}

func TestLoadChainWatcherDefaults(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_SOURCE_POLL_SECONDS", "")
	t.Setenv("CHAIN_WATCHER_GLOBAL_SCAN_PAGES", "")
	t.Setenv("TRONSCAN_GLOBAL_SCAN_PAGES", "")
	t.Setenv("CHAIN_WATCHER_ADDRESS_SCAN_INTERVAL_SECONDS", "")
	t.Setenv("CHAIN_WATCHER_ADDRESS_SCAN_PAGES", "")
	t.Setenv("CHAIN_WATCHER_ADDRESS_SCAN_CONCURRENCY", "")
	t.Setenv("CHAIN_WATCHER_ADDRESS_SCAN_MAX_PER_TICK", "")
	t.Setenv("CHAIN_WATCHER_TRON_REQUEST_INTERVAL_MS", "")
	cfg, err := LoadChainWatcher()
	if err != nil {
		t.Fatalf("LoadChainWatcher() error = %v", err)
	}
	if got := cfg.PollInterval.Seconds(); got != 1 {
		t.Fatalf("PollInterval = %.0f seconds, want 1", got)
	}
	if cfg.GlobalPages != 3 {
		t.Fatalf("GlobalPages = %d, want 3", cfg.GlobalPages)
	}
	if got := cfg.AddressInterval.Seconds(); got != 30 {
		t.Fatalf("AddressInterval = %.0f seconds, want 30", got)
	}
	if cfg.AddressPages != 1 {
		t.Fatalf("AddressPages = %d, want 1", cfg.AddressPages)
	}
	if cfg.AddressConcurrency != 1 {
		t.Fatalf("AddressConcurrency = %d, want 1", cfg.AddressConcurrency)
	}
	if cfg.AddressMaxPerTick != 1 {
		t.Fatalf("AddressMaxPerTick = %d, want 1", cfg.AddressMaxPerTick)
	}
	if got := cfg.RequestInterval.Milliseconds(); got != 250 {
		t.Fatalf("RequestInterval = %d ms, want 250", got)
	}
}

func TestLoadBotWatcherFallbackDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("BOT_WATCHER_HEALTH_INTERVAL_SECONDS", "")
	t.Setenv("BOT_WATCHER_FAIL_THRESHOLD", "")
	t.Setenv("BOT_WATCHER_CLAIM_TIMEOUT_MS", "")
	t.Setenv("BOT_FALLBACK_POLL_SECONDS", "")
	t.Setenv("BOT_FALLBACK_MAX_ACTIVE_SECONDS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.BotWatcherHealthInterval.Seconds(); got != 1 {
		t.Fatalf("BotWatcherHealthInterval = %.0f seconds, want 1", got)
	}
	if cfg.BotWatcherFailThreshold != 2 {
		t.Fatalf("BotWatcherFailThreshold = %d, want 2", cfg.BotWatcherFailThreshold)
	}
	if got := cfg.BotWatcherClaimTimeout.Milliseconds(); got != 2000 {
		t.Fatalf("BotWatcherClaimTimeout = %d ms, want 2000", got)
	}
	if got := cfg.BotFallbackPollInterval.Seconds(); got != 1 {
		t.Fatalf("BotFallbackPollInterval = %.0f seconds, want 1", got)
	}
	if got := cfg.BotFallbackMaxActive.Seconds(); got != 600 {
		t.Fatalf("BotFallbackMaxActive = %.0f seconds, want 600", got)
	}
}

func TestLoadSlowUpdateThreshold(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("BOT_SLOW_UPDATE_THRESHOLD_MS", "1200")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.SlowUpdateThreshold.Milliseconds(); got != 1200 {
		t.Fatalf("SlowUpdateThreshold = %d ms, want 1200", got)
	}
}

func TestLoadBillSummaryCacheTTL(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("BOT_BILL_SUMMARY_CACHE_TTL_SECONDS", "45")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.BillSummaryCacheTTL.Seconds(); got != 45 {
		t.Fatalf("BillSummaryCacheTTL = %.0f seconds, want 45", got)
	}
}

func TestLoadOutboxRetentionDefaultsAndEnv(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("BOT_OUTBOX_SENT_RETENTION_HOURS", "")
	t.Setenv("BOT_OUTBOX_FAILED_RETENTION_HOURS", "")
	t.Setenv("BOT_OUTBOX_STATS_WINDOW_HOURS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.OutboxSentRetention.Hours(); got != 72 {
		t.Fatalf("OutboxSentRetention = %.0f hours, want 72", got)
	}
	if got := cfg.OutboxFailedRetention.Hours(); got != 24*14 {
		t.Fatalf("OutboxFailedRetention = %.0f hours, want %d", got, 24*14)
	}
	if got := cfg.OutboxStatsWindow.Hours(); got != 72 {
		t.Fatalf("OutboxStatsWindow = %.0f hours, want 72", got)
	}

	t.Setenv("BOT_OUTBOX_SENT_RETENTION_HOURS", "24")
	t.Setenv("BOT_OUTBOX_FAILED_RETENTION_HOURS", "168")
	t.Setenv("BOT_OUTBOX_STATS_WINDOW_HOURS", "12")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.OutboxSentRetention.Hours(); got != 24 {
		t.Fatalf("OutboxSentRetention env = %.0f hours, want 24", got)
	}
	if got := cfg.OutboxFailedRetention.Hours(); got != 168 {
		t.Fatalf("OutboxFailedRetention env = %.0f hours, want 168", got)
	}
	if got := cfg.OutboxStatsWindow.Hours(); got != 12 {
		t.Fatalf("OutboxStatsWindow env = %.0f hours, want 12", got)
	}
}
