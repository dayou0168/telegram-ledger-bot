package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dayou0168/telegram-ledger-bot/go-ledger/internal/tron"
)

const Version = "2.4.4"

type Config struct {
	TelegramBotToken string
	TelegramAPIBase  string
	TelegramUsername string
	DatabaseURL      string
	Timezone         string

	HostUserID         int64
	DefaultOperatorIDs map[int64]struct{}

	LedgerWorkers              int
	ControlWorkers             int
	ChainWorkers               int
	RateWorkers                int
	BroadcastWorkers           int
	QueryWorkers               int
	NotifyWorkers              int
	QueueSize                  int
	OutboxSentRetention        time.Duration
	OutboxFailedRetention      time.Duration
	OutboxStatsWindow          time.Duration
	BroadcastDeliveryRetention time.Duration
	GroupCacheTTL              time.Duration
	BillSummaryCacheTTL        time.Duration
	UserTouchCacheTTL          time.Duration
	OperatorCacheTTL           time.Duration
	WatchCacheTTL              time.Duration
	SlowUpdateThreshold        time.Duration
	PollTimeout                time.Duration
	RequestTimeout             time.Duration
	TronBackfillEvery          time.Duration
	TronLookbackMinutes        int

	TronAPIBase                   string
	TronAPIKey                    string
	USDTContract                  string
	TronGlobalPages               int
	ChainWatcherURL               string
	ChainWatcherBotID             string
	ChainWatcherSecret            string
	ChainWatcherPollInterval      time.Duration
	ChainWatcherBatchSize         int
	BotWatcherHealthInterval      time.Duration
	BotWatcherFailThreshold       int
	BotWatcherClaimTimeout        time.Duration
	BotFallbackPollInterval       time.Duration
	BotFallbackSharedDatabaseURL  string
	BotFallbackInstanceID         string
	BotFallbackLeaseTTL           time.Duration
	BotFallbackGlobalPages        int
	BotFallbackRequestInterval    time.Duration
	BotFallbackRecoverySuccesses  int
	BotFallbackRecoveryLag        time.Duration
	BotFallbackMaxRequestsPerTick int
	BotFallbackWindow             time.Duration
	P2PRefreshEvery               time.Duration
	P2PCacheTTL                   time.Duration
	P2PAPIBase                    string
	P2PFrontAPI                   string
	P2PMarket                     string
	P2PFiatUnit                   string
	P2PAsset                      string
	P2PTradeMethods               []string
	PublicBillBaseURL             string
	AdminWebEnabled               bool
	AdminWebHost                  string
	AdminWebPort                  int
	AdminWebToken                 string
	AdminWebCookieSecure          bool
	AddressWatchFreeLimit         int
}

func Load() (Config, error) {
	publicBillBaseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_BILL_BASE_URL")), "/")
	cfg := Config{
		TelegramBotToken:              strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		TelegramAPIBase:               env("TELEGRAM_API_BASE", "https://api.telegram.org"),
		TelegramUsername:              strings.TrimPrefix(strings.TrimSpace(os.Getenv("TELEGRAM_BOT_USERNAME")), "@"),
		DatabaseURL:                   envAny([]string{"DATABASE_URL", "POSTGRES_DSN"}, "postgres://ledger:ledger@127.0.0.1:5432/ledger_bot?sslmode=disable"),
		Timezone:                      env("BOT_TIMEZONE", "Asia/Shanghai"),
		HostUserID:                    int64Env("BOT_HOST_USER_ID", 0),
		DefaultOperatorIDs:            parseIDs(os.Getenv("DEFAULT_OPERATOR_USER_IDS")),
		LedgerWorkers:                 intEnv("BOT_WORKER_THREADS", 16),
		ControlWorkers:                intEnv("BOT_CONTROL_THREADS", 6),
		ChainWorkers:                  intEnv("BOT_CHAIN_THREADS", 12),
		RateWorkers:                   intEnv("BOT_RATE_THREADS", 1),
		BroadcastWorkers:              intEnv("BOT_BROADCAST_THREADS", 4),
		QueryWorkers:                  intEnv("BOT_QUERY_THREADS", 4),
		NotifyWorkers:                 intEnv("BOT_NOTIFICATION_THREADS", 6),
		QueueSize:                     intEnv("BOT_QUEUE_SIZE", 4096),
		OutboxSentRetention:           hoursEnv("BOT_OUTBOX_SENT_RETENTION_HOURS", 72),
		OutboxFailedRetention:         hoursEnv("BOT_OUTBOX_FAILED_RETENTION_HOURS", 24*14),
		OutboxStatsWindow:             hoursEnv("BOT_OUTBOX_STATS_WINDOW_HOURS", 72),
		BroadcastDeliveryRetention:    hoursEnv("BOT_BROADCAST_DELIVERY_RETENTION_HOURS", 168),
		GroupCacheTTL:                 secondsEnv("BOT_GROUP_CACHE_TTL_SECONDS", 60),
		BillSummaryCacheTTL:           secondsEnv("BOT_BILL_SUMMARY_CACHE_TTL_SECONDS", 30),
		UserTouchCacheTTL:             secondsEnv("BOT_USER_TOUCH_CACHE_TTL_SECONDS", 180),
		OperatorCacheTTL:              secondsEnv("BOT_OPERATOR_CACHE_TTL_SECONDS", 10),
		WatchCacheTTL:                 secondsEnv("BOT_WATCH_CACHE_TTL_SECONDS", 3),
		SlowUpdateThreshold:           millisEnv("BOT_SLOW_UPDATE_THRESHOLD_MS", 800),
		PollTimeout:                   secondsEnv("BOT_POLL_TIMEOUT", 50),
		RequestTimeout:                secondsEnv("BOT_REQUEST_TIMEOUT", 70),
		TronBackfillEvery:             secondsEnv("TRON_ADDRESS_BACKFILL_SECONDS", 60),
		TronLookbackMinutes:           intEnv("TRON_INITIAL_LOOKBACK_MINUTES", 15),
		TronAPIBase:                   strings.TrimRight(envAny([]string{"TRONSCAN_API_BASE", "TRONGRID_API_BASE"}, "https://apilist.tronscanapi.com/api"), "/"),
		TronAPIKey:                    strings.TrimSpace(os.Getenv("TRONGRID_API_KEY")),
		USDTContract:                  env("TRON_USDT_CONTRACT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"),
		TronGlobalPages:               intEnv("TRONSCAN_GLOBAL_SCAN_PAGES", 1),
		ChainWatcherURL:               strings.TrimRight(strings.TrimSpace(os.Getenv("CHAIN_WATCHER_URL")), "/"),
		ChainWatcherBotID:             strings.TrimSpace(os.Getenv("CHAIN_WATCHER_BOT_ID")),
		ChainWatcherSecret:            strings.TrimSpace(os.Getenv("CHAIN_WATCHER_SECRET")),
		ChainWatcherPollInterval:      secondsEnv("CHAIN_WATCHER_POLL_SECONDS", 1),
		ChainWatcherBatchSize:         intEnv("CHAIN_WATCHER_BATCH_SIZE", 50),
		BotWatcherHealthInterval:      secondsEnv("BOT_WATCHER_HEALTH_INTERVAL_SECONDS", 1),
		BotWatcherFailThreshold:       intEnv("BOT_WATCHER_FAIL_THRESHOLD", 3),
		BotWatcherClaimTimeout:        millisEnv("BOT_WATCHER_CLAIM_TIMEOUT_MS", 2000),
		BotFallbackPollInterval:       secondsEnv("BOT_FALLBACK_POLL_SECONDS", 1),
		BotFallbackSharedDatabaseURL:  strings.TrimSpace(os.Getenv("BOT_FALLBACK_SHARED_DATABASE_URL")),
		BotFallbackInstanceID:         strings.TrimSpace(os.Getenv("BOT_FALLBACK_INSTANCE_ID")),
		BotFallbackLeaseTTL:           secondsEnv("BOT_FALLBACK_LEASE_SECONDS", 15),
		BotFallbackGlobalPages:        intEnv("BOT_FALLBACK_GLOBAL_PAGES", 3),
		BotFallbackRequestInterval:    millisEnv("BOT_FALLBACK_REQUEST_INTERVAL_MS", 0),
		BotFallbackRecoverySuccesses:  intEnv("BOT_FALLBACK_RECOVERY_SUCCESS_ROUNDS", 3),
		BotFallbackRecoveryLag:        secondsEnv("BOT_FALLBACK_RECOVERY_LAG_SECONDS", 5),
		BotFallbackMaxRequestsPerTick: intEnv("BOT_FALLBACK_MAX_REQUESTS_PER_TICK", 6),
		BotFallbackWindow:             secondsEnv("BOT_FALLBACK_WINDOW_SECONDS", 30),
		P2PRefreshEvery:               secondsEnv("P2P_RATE_REFRESH_SECONDS", 60),
		P2PCacheTTL:                   secondsEnv("P2P_RATE_CACHE_TTL_SECONDS", 180),
		P2PAPIBase:                    strings.TrimRight(env("P2P_RATE_API_BASE", "https://p2p.army/api/fapi"), "/"),
		P2PFrontAPI:                   env("P2P_RATE_FRONT_API", "NextVOF2Ozuh36mW0TCv"),
		P2PMarket:                     env("P2P_RATE_MARKET", "okx"),
		P2PFiatUnit:                   env("P2P_RATE_FIAT_UNIT", "CNY"),
		P2PAsset:                      env("P2P_RATE_ASSET", "USDT"),
		P2PTradeMethods:               parseCSV(env("P2P_RATE_TRADE_METHODS", "aliPay")),
		PublicBillBaseURL:             publicBillBaseURL,
		AdminWebEnabled:               boolEnv("ADMIN_WEB_ENABLED", true),
		AdminWebHost:                  env("ADMIN_WEB_HOST", "0.0.0.0"),
		AdminWebPort:                  intEnv("ADMIN_WEB_PORT", 8080),
		AdminWebToken:                 strings.TrimSpace(os.Getenv("ADMIN_WEB_TOKEN")),
		AddressWatchFreeLimit:         intEnv("ADDRESS_WATCH_FREE_LIMIT", 2),
	}
	if raw := strings.TrimSpace(os.Getenv("ADMIN_WEB_COOKIE_SECURE")); raw != "" {
		cfg.AdminWebCookieSecure = boolEnv("ADMIN_WEB_COOKIE_SECURE", false)
	} else {
		cfg.AdminWebCookieSecure = strings.HasPrefix(strings.ToLower(cfg.PublicBillBaseURL), "https://")
	}
	if cfg.TelegramBotToken == "" {
		return cfg, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.LedgerWorkers < 1 {
		cfg.LedgerWorkers = 1
	}
	if cfg.ControlWorkers < 1 {
		cfg.ControlWorkers = 1
	}
	if cfg.QueueSize < 128 {
		cfg.QueueSize = 128
	}
	if cfg.OutboxSentRetention <= 0 {
		cfg.OutboxSentRetention = 72 * time.Hour
	}
	if cfg.OutboxFailedRetention <= 0 {
		cfg.OutboxFailedRetention = 14 * 24 * time.Hour
	}
	if cfg.OutboxStatsWindow <= 0 {
		cfg.OutboxStatsWindow = 72 * time.Hour
	}
	if cfg.BroadcastDeliveryRetention <= 0 {
		cfg.BroadcastDeliveryRetention = 168 * time.Hour
	}
	if cfg.ChainWatcherBatchSize < 1 {
		cfg.ChainWatcherBatchSize = 1
	}
	if cfg.BotWatcherHealthInterval <= 0 {
		cfg.BotWatcherHealthInterval = time.Second
	}
	if cfg.BotWatcherFailThreshold < 1 {
		cfg.BotWatcherFailThreshold = 1
	}
	if cfg.BotWatcherClaimTimeout <= 0 {
		cfg.BotWatcherClaimTimeout = 2 * time.Second
	}
	if cfg.BotFallbackPollInterval <= 0 {
		cfg.BotFallbackPollInterval = time.Second
	}
	if cfg.BotFallbackLeaseTTL <= 0 {
		cfg.BotFallbackLeaseTTL = 15 * time.Second
	}
	if cfg.BotFallbackGlobalPages < 1 {
		cfg.BotFallbackGlobalPages = 1
	}
	if cfg.BotFallbackRequestInterval < 0 {
		cfg.BotFallbackRequestInterval = 0
	}
	if cfg.BotFallbackRecoverySuccesses < 2 {
		cfg.BotFallbackRecoverySuccesses = 2
	}
	if cfg.BotFallbackRecoveryLag <= 0 {
		cfg.BotFallbackRecoveryLag = 5 * time.Second
	}
	if cfg.BotFallbackMaxRequestsPerTick < 1 {
		cfg.BotFallbackMaxRequestsPerTick = 1
	}
	if cfg.BotFallbackWindow <= 0 {
		cfg.BotFallbackWindow = 30 * time.Second
	}
	if cfg.SlowUpdateThreshold <= 0 {
		cfg.SlowUpdateThreshold = 800 * time.Millisecond
	}
	if cfg.BillSummaryCacheTTL <= 0 {
		cfg.BillSummaryCacheTTL = 30 * time.Second
	}
	if cfg.AddressWatchFreeLimit < 0 {
		cfg.AddressWatchFreeLimit = 0
	}
	return cfg, nil
}

func (cfg Config) ChainWatcherEnabled() bool {
	return cfg.ChainWatcherURL != "" && cfg.ChainWatcherBotID != "" && cfg.ChainWatcherSecret != ""
}

func (cfg Config) SharedFallbackEnabled() bool {
	return cfg.ChainWatcherEnabled() && cfg.BotFallbackSharedDatabaseURL != ""
}

type ChainWatcherConfig struct {
	DatabaseURL             string
	ListenAddr              string
	AdminToken              string
	KeyEncryptionKey        string
	Timezone                string
	RequestTimeout          time.Duration
	TronAPIBase             string
	TronAPIKey              string
	TronAPIKeys             []string
	USDTContract            string
	PollInterval            time.Duration
	MainScanTimeout         time.Duration
	MainMaxInflight         int
	GlobalPages             int
	GlobalExpandPageLimit   int
	CatchupInterval         time.Duration
	CatchupEnabled          bool
	CatchupPages            int
	CatchupMaxRequests      int
	CatchupMaxInflight      int
	CatchupWindow           time.Duration
	CatchupOverlap          time.Duration
	CatchupMaxRPS           float64
	CatchupTargetLag        time.Duration
	CatchupMaxLag           time.Duration
	RequestInterval         time.Duration
	KeyAuthProbeInterval    time.Duration
	KeyInvalidProbeInterval time.Duration
	KeyBlockedProbeInterval time.Duration
	BudgetTimezone          string
	BudgetLocation          *time.Location
	Lookback                time.Duration
	BotCredentials          map[string]string
	ClaimLease              time.Duration
	DeliveryRetryEvery      time.Duration
}

func LoadChainWatcher() (ChainWatcherConfig, error) {
	cfg := ChainWatcherConfig{
		DatabaseURL:             envAny([]string{"CHAIN_WATCHER_DATABASE_URL", "DATABASE_URL", "POSTGRES_DSN"}, "postgres://ledger:ledger@127.0.0.1:5432/ledger_bot?sslmode=disable"),
		ListenAddr:              env("CHAIN_WATCHER_ADDR", ":8090"),
		AdminToken:              strings.TrimSpace(os.Getenv("CHAIN_WATCHER_ADMIN_TOKEN")),
		KeyEncryptionKey:        strings.TrimSpace(os.Getenv("CHAIN_WATCHER_KEY_ENCRYPTION_KEY")),
		Timezone:                env("BOT_TIMEZONE", "Asia/Shanghai"),
		RequestTimeout:          secondsEnv("BOT_REQUEST_TIMEOUT", 70),
		TronAPIBase:             strings.TrimRight(envAny([]string{"CHAIN_WATCHER_TRONSCAN_API_BASE", "TRONSCAN_API_BASE", "TRONGRID_API_BASE"}, "https://apilist.tronscanapi.com/api"), "/"),
		TronAPIKey:              strings.TrimSpace(envAny([]string{"CHAIN_WATCHER_TRONSCAN_API_KEY", "CHAIN_WATCHER_TRON_API_KEY", "TRONGRID_API_KEY"}, "")),
		TronAPIKeys:             parseListEnv(os.Getenv("CHAIN_WATCHER_TRONSCAN_API_KEYS")),
		USDTContract:            env("TRON_USDT_CONTRACT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"),
		PollInterval:            secondsEnv("CHAIN_WATCHER_SOURCE_POLL_SECONDS", 1),
		MainScanTimeout:         millisEnv("CHAIN_WATCHER_MAIN_SCAN_TIMEOUT_MS", 3000),
		MainMaxInflight:         intEnv("CHAIN_WATCHER_MAIN_MAX_INFLIGHT_ROUNDS", 3),
		GlobalPages:             intEnv("CHAIN_WATCHER_GLOBAL_SCAN_PAGES", intEnv("TRONSCAN_GLOBAL_SCAN_PAGES", 3)),
		GlobalExpandPageLimit:   intEnv("CHAIN_WATCHER_GLOBAL_EXPAND_PAGE_LIMIT", 20),
		CatchupInterval:         secondsEnv("CHAIN_WATCHER_CATCHUP_STATE_INTERVAL_SECONDS", 30),
		CatchupEnabled:          boolEnv("CHAIN_WATCHER_CATCHUP_ENABLED", true),
		CatchupPages:            intEnv("CHAIN_WATCHER_CATCHUP_PAGE_LIMIT", 3),
		CatchupMaxRequests:      intEnv("CHAIN_WATCHER_CATCHUP_MAX_REQUESTS_PER_TICK", 6),
		CatchupMaxInflight:      intEnv("CHAIN_WATCHER_CATCHUP_MAX_INFLIGHT", 3),
		CatchupWindow:           secondsEnv("CHAIN_WATCHER_CATCHUP_WINDOW_SECONDS", 30),
		CatchupOverlap:          secondsEnv("CHAIN_WATCHER_CATCHUP_OVERLAP_SECONDS", 2),
		CatchupMaxRPS:           floatEnv("CHAIN_WATCHER_CATCHUP_MAX_RPS", 8),
		CatchupTargetLag:        secondsEnv("CHAIN_WATCHER_CATCHUP_TARGET_LAG_SECONDS", 30),
		CatchupMaxLag:           secondsEnv("CHAIN_WATCHER_CATCHUP_MAX_LAG_SECONDS", 120),
		RequestInterval:         millisEnv("CHAIN_WATCHER_TRONSCAN_KEY_INTERVAL_MS", int(millisEnv("CHAIN_WATCHER_TRON_REQUEST_INTERVAL_MS", 200)/time.Millisecond)),
		KeyAuthProbeInterval:    secondsEnv("CHAIN_WATCHER_KEY_AUTH_PROBE_SECONDS", 5),
		KeyInvalidProbeInterval: secondsEnv("CHAIN_WATCHER_KEY_INVALID_PROBE_SECONDS", 1800),
		KeyBlockedProbeInterval: secondsEnv("CHAIN_WATCHER_KEY_BLOCKED_PROBE_SECONDS", 3600),
		BudgetTimezone:          "UTC",
		Lookback:                secondsEnv("CHAIN_WATCHER_LOOKBACK_SECONDS", 600),
		BotCredentials:          parseBotCredentials(os.Getenv("CHAIN_WATCHER_BOTS")),
		ClaimLease:              secondsEnv("CHAIN_WATCHER_CLAIM_LEASE_SECONDS", 30),
		DeliveryRetryEvery:      secondsEnv("CHAIN_WATCHER_DELIVERY_RETRY_SECONDS", 2),
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.KeyEncryptionKey == "" {
		return cfg, errors.New("CHAIN_WATCHER_KEY_ENCRYPTION_KEY is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MainScanTimeout < time.Second {
		cfg.MainScanTimeout = 3 * time.Second
	}
	if cfg.MainMaxInflight < 1 {
		cfg.MainMaxInflight = 1
	}
	if cfg.MainMaxInflight > 3 {
		cfg.MainMaxInflight = 3
	}
	if cfg.GlobalPages < 1 {
		cfg.GlobalPages = 1
	}
	if cfg.GlobalExpandPageLimit < cfg.GlobalPages {
		cfg.GlobalExpandPageLimit = 20
	}
	if len(cfg.TronAPIKeys) == 0 && cfg.TronAPIKey != "" {
		cfg.TronAPIKeys = []string{cfg.TronAPIKey}
	}
	if len(cfg.TronAPIKeys) > tron.MaxConfiguredKeys {
		return cfg, fmt.Errorf("CHAIN_WATCHER_TRONSCAN_API_KEYS supports at most %d keys; got %d", tron.MaxConfiguredKeys, len(cfg.TronAPIKeys))
	}
	if cfg.CatchupInterval <= 0 {
		cfg.CatchupInterval = 30 * time.Second
	}
	if cfg.CatchupPages < 1 {
		cfg.CatchupPages = 1
	}
	if cfg.CatchupMaxRequests < 1 {
		cfg.CatchupMaxRequests = 1
	}
	if cfg.CatchupMaxInflight < 1 {
		cfg.CatchupMaxInflight = 1
	}
	if cfg.CatchupMaxInflight > 3 {
		cfg.CatchupMaxInflight = 3
	}
	if cfg.CatchupWindow <= 0 {
		cfg.CatchupWindow = 30 * time.Second
	}
	if cfg.CatchupOverlap < 0 {
		cfg.CatchupOverlap = 0
	}
	if cfg.CatchupMaxRPS <= 0 {
		cfg.CatchupMaxRPS = 8
	}
	if cfg.CatchupTargetLag <= 0 {
		cfg.CatchupTargetLag = 30 * time.Second
	}
	if cfg.CatchupMaxLag < cfg.CatchupTargetLag {
		cfg.CatchupMaxLag = 2 * time.Minute
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 10 * time.Minute
	}
	if cfg.RequestInterval < 200*time.Millisecond {
		cfg.RequestInterval = 200 * time.Millisecond
	}
	if cfg.KeyAuthProbeInterval <= 0 {
		cfg.KeyAuthProbeInterval = 5 * time.Second
	}
	if cfg.KeyInvalidProbeInterval <= 0 {
		cfg.KeyInvalidProbeInterval = 30 * time.Minute
	}
	if cfg.KeyBlockedProbeInterval <= 0 {
		cfg.KeyBlockedProbeInterval = time.Hour
	}
	cfg.BudgetLocation = time.UTC
	if cfg.ClaimLease <= 0 {
		cfg.ClaimLease = 30 * time.Second
	}
	if cfg.DeliveryRetryEvery <= 0 {
		cfg.DeliveryRetryEvery = 2 * time.Second
	}
	return cfg, nil
}

func env(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envAny(keys []string, fallback string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return fallback
}

func parseListEnv(raw string) []string {
	seen := make(map[string]struct{})
	var values []string
	for _, value := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	}) {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

func intEnv(key string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}

func floatEnv(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

func int64Env(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func boolEnv(key string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func secondsEnv(key string, fallback int) time.Duration {
	return time.Duration(intEnv(key, fallback)) * time.Second
}

func millisEnv(key string, fallback int) time.Duration {
	return time.Duration(intEnv(key, fallback)) * time.Millisecond
}

func hoursEnv(key string, fallback int) time.Duration {
	return time.Duration(intEnv(key, fallback)) * time.Hour
}

func parseIDs(raw string) map[int64]struct{} {
	ids := make(map[int64]struct{})
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	})
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err == nil && id > 0 {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func parseCSV(raw string) []string {
	var values []string
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
	}) {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, part)
		}
	}
	return values
}

func parseBotCredentials(raw string) map[string]string {
	out := make(map[string]string)
	for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t'
	}) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, ":")
		if !ok {
			key, value, ok = strings.Cut(part, "=")
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if ok && key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}
