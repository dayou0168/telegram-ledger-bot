package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoadChainWatcherDefaults(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CHAIN_WATCHER_SOURCE_POLL_SECONDS", "")
	t.Setenv("CHAIN_WATCHER_MAIN_SCAN_TIMEOUT_MS", "")
	t.Setenv("CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS", "")
	t.Setenv("CHAIN_WATCHER_CATCHUP_MAX_INFLIGHT", "")
	t.Setenv("CHAIN_WATCHER_CATCHUP_MAX_RPS", "")
	t.Setenv("CHAIN_WATCHER_GLOBAL_SCAN_PAGES", "")
	t.Setenv("TRONSCAN_GLOBAL_SCAN_PAGES", "")
	t.Setenv("CHAIN_WATCHER_HEAD_TIME_BUDGET_MS", "")
	t.Setenv("CHAIN_WATCHER_HEAD_SAFETY_MAX_PAGES", "")
	t.Setenv("CHAIN_WATCHER_HEAD_MAX_CONCURRENCY", "")
	t.Setenv("CHAIN_WATCHER_HEAD_PERSIST_CONCURRENCY", "")
	t.Setenv("CHAIN_WATCHER_RECOVERY_SAFETY_MAX_PAGES", "")
	t.Setenv("CHAIN_WATCHER_SURPLUS_BURST_SECONDS", "")
	t.Setenv("CHAIN_WATCHER_TRON_REQUEST_INTERVAL_MS", "")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_KEY_INTERVAL_MS", "")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_DAILY_LIMIT_PER_KEY", "")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_HARD_LIMIT_PER_KEY", "")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_BUDGET_TIMEZONE", "")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_API_KEYS", "")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_API_KEY", "")
	t.Setenv("CHAIN_WATCHER_TRON_API_KEY", "")
	t.Setenv("TRONGRID_API_KEY", "")
	cfg, err := LoadChainWatcher()
	if err != nil {
		t.Fatalf("LoadChainWatcher() error = %v", err)
	}
	if got := cfg.PollInterval.Seconds(); got != 1 {
		t.Fatalf("PollInterval = %.0f seconds, want 1", got)
	}
	if cfg.DeprecatedGlobalPagesConfigured || cfg.DeprecatedGlobalPages != 0 {
		t.Fatalf("deprecated global pages = %d/%v, want 0/false", cfg.DeprecatedGlobalPages, cfg.DeprecatedGlobalPagesConfigured)
	}
	if cfg.MainScanTimeout != 3*time.Second || cfg.MainMaxInflight != 3 {
		t.Fatalf("main scan timeout/inflight = %v/%d, want 3s/3", cfg.MainScanTimeout, cfg.MainMaxInflight)
	}
	if cfg.CatchupMaxInflight != 8 || cfg.GapFairnessEvery != 4 {
		t.Fatalf("catchup inflight/fairness = %d/%d, want 8/4", cfg.CatchupMaxInflight, cfg.GapFairnessEvery)
	}
	if cfg.CatchupMaxRPS != 0 {
		t.Fatalf("CatchupMaxRPS = %v, want dynamic surplus without an extra cap", cfg.CatchupMaxRPS)
	}
	if cfg.KeyDailyQuota != 100000 {
		t.Fatalf("KeyDailyQuota = %d, want 100000", cfg.KeyDailyQuota)
	}
	if cfg.HeadTimeBudget != 850*time.Millisecond || cfg.HeadSafetyMaxPages != 256 || cfg.RecoverySafetyMaxPages != 4096 {
		t.Fatalf("head safety defaults = %v/%d/%d", cfg.HeadTimeBudget, cfg.HeadSafetyMaxPages, cfg.RecoverySafetyMaxPages)
	}
	if cfg.HeadMaxConcurrency != 32 || cfg.HeadPersistConcurrency != 8 {
		t.Fatalf("head concurrency defaults = api %d persist %d", cfg.HeadMaxConcurrency, cfg.HeadPersistConcurrency)
	}
	if cfg.SurplusBurstWindow != 60*time.Second || cfg.CatchupOverlap != 2*time.Second || cfg.CatchupInterval != 30*time.Second {
		t.Fatalf("budget/catchup defaults = %v/%v/%v", cfg.SurplusBurstWindow, cfg.CatchupOverlap, cfg.CatchupInterval)
	}
	if got := cfg.RequestInterval.Milliseconds(); got != 200 {
		t.Fatalf("RequestInterval = %d ms, want 200", got)
	}
	if cfg.BudgetTimezone != "UTC" {
		t.Fatalf("budget timezone = %s, want UTC", cfg.BudgetTimezone)
	}
}

func TestLoadChainWatcherClampsInflightRoundsToThree(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS", "99")
	cfg, err := LoadChainWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MainMaxInflight != 3 {
		t.Fatalf("MainMaxInflight = %d, want 3", cfg.MainMaxInflight)
	}
}

func TestLoadChainWatcherClampsCatchupInflightToSafetyCeiling(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CHAIN_WATCHER_CATCHUP_MAX_INFLIGHT", "99")
	cfg, err := LoadChainWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CatchupMaxInflight != 64 {
		t.Fatalf("CatchupMaxInflight = %d, want 64", cfg.CatchupMaxInflight)
	}
}

func TestLoadChainWatcherAPIKeyPoolAndLegacyCompatibility(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_API_KEYS", " key1, key2\nkey1 ")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_API_KEY", "legacy")
	cfg, err := LoadChainWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.TronAPIKeys, ","); got != "key1,key2" {
		t.Fatalf("pooled keys = %q, want key1,key2", got)
	}

	t.Setenv("CHAIN_WATCHER_TRONSCAN_API_KEYS", "")
	cfg, err = LoadChainWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.TronAPIKeys, ","); got != "legacy" {
		t.Fatalf("legacy keys = %q, want legacy", got)
	}
}

func TestLoadChainWatcherAcceptsDynamicAPIKeyPool(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	keys := make([]string, 30)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%d", i)
	}
	t.Setenv("CHAIN_WATCHER_TRONSCAN_API_KEYS", strings.Join(keys, ","))
	cfg, err := LoadChainWatcher()
	if err != nil || len(cfg.TronAPIKeys) != 30 {
		t.Fatalf("LoadChainWatcher keys/error = %d/%v, want 30/nil", len(cfg.TronAPIKeys), err)
	}
}

func TestLoadChainWatcherLegacyRequestIntervalFallback(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("CHAIN_WATCHER_TRONSCAN_KEY_INTERVAL_MS", "")
	t.Setenv("CHAIN_WATCHER_TRON_REQUEST_INTERVAL_MS", "375")
	cfg, err := LoadChainWatcher()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.RequestInterval.Milliseconds(); got != 375 {
		t.Fatalf("request interval = %d, want 375", got)
	}
}

func TestLoadChainWatcherRequiresEncryptionKey(t *testing.T) {
	t.Setenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY", "")
	if _, err := LoadChainWatcher(); err == nil || !strings.Contains(err.Error(), "KEY_ENCRYPTION_KEY") {
		t.Fatalf("error = %v, want encryption key requirement", err)
	}
}

func TestLoadBotWatcherFallbackDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("BOT_WATCHER_HEALTH_INTERVAL_SECONDS", "")
	t.Setenv("BOT_WATCHER_FAIL_THRESHOLD", "")
	t.Setenv("BOT_WATCHER_CLAIM_TIMEOUT_MS", "")
	t.Setenv("BOT_FALLBACK_POLL_SECONDS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.BotWatcherHealthInterval.Seconds(); got != 1 {
		t.Fatalf("BotWatcherHealthInterval = %.0f seconds, want 1", got)
	}
	if cfg.BotWatcherFailThreshold != 3 {
		t.Fatalf("BotWatcherFailThreshold = %d, want 3", cfg.BotWatcherFailThreshold)
	}
	if got := cfg.BotWatcherClaimTimeout.Milliseconds(); got != 2000 {
		t.Fatalf("BotWatcherClaimTimeout = %d ms, want 2000", got)
	}
	if got := cfg.BotFallbackPollInterval.Seconds(); got != 1 {
		t.Fatalf("BotFallbackPollInterval = %.0f seconds, want 1", got)
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
	t.Setenv("BOT_BROADCAST_DELIVERY_RETENTION_HOURS", "")
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
	if got := cfg.BroadcastDeliveryRetention.Hours(); got != 168 {
		t.Fatalf("BroadcastDeliveryRetention = %.0f hours, want 168", got)
	}

	t.Setenv("BOT_OUTBOX_SENT_RETENTION_HOURS", "24")
	t.Setenv("BOT_OUTBOX_FAILED_RETENTION_HOURS", "168")
	t.Setenv("BOT_OUTBOX_STATS_WINDOW_HOURS", "12")
	t.Setenv("BOT_BROADCAST_DELIVERY_RETENTION_HOURS", "240")
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
	if got := cfg.BroadcastDeliveryRetention.Hours(); got != 240 {
		t.Fatalf("BroadcastDeliveryRetention env = %.0f hours, want 240", got)
	}
}
