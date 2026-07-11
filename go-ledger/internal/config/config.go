package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

const Version = "2.3.4"

type Config struct {
	TelegramBotToken string
	TelegramAPIBase  string
	TelegramUsername string
	DatabaseURL      string
	Timezone         string

	HostUserID         int64
	DefaultOperatorIDs map[int64]struct{}

	LedgerWorkers       int
	ControlWorkers      int
	ChainWorkers        int
	RateWorkers         int
	BroadcastWorkers    int
	QueryWorkers        int
	NotifyWorkers       int
	QueueSize           int
	GroupCacheTTL       time.Duration
	UserTouchCacheTTL   time.Duration
	OperatorCacheTTL    time.Duration
	WatchCacheTTL       time.Duration
	PollTimeout         time.Duration
	RequestTimeout      time.Duration
	TronPollInterval    time.Duration
	TronBackfillEvery   time.Duration
	TronLookbackMinutes int

	TronAPIBase                   string
	TronAPIKey                    string
	USDTContract                  string
	TronGlobalPages               int
	TronAddressPages              int
	TronAddressScanConcurrency    int
	ChainWatcherURL               string
	ChainWatcherBotID             string
	ChainWatcherSecret            string
	ChainWatcherPollInterval      time.Duration
	ChainWatcherBatchSize         int
	ChainWatcherEmergencyFallback bool
	BotWatcherHealthInterval      time.Duration
	BotWatcherFailThreshold       int
	BotWatcherClaimTimeout        time.Duration
	BotFallbackPollInterval       time.Duration
	BotFallbackMaxActive          time.Duration
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
		GroupCacheTTL:                 secondsEnv("BOT_GROUP_CACHE_TTL_SECONDS", 60),
		UserTouchCacheTTL:             secondsEnv("BOT_USER_TOUCH_CACHE_TTL_SECONDS", 180),
		OperatorCacheTTL:              secondsEnv("BOT_OPERATOR_CACHE_TTL_SECONDS", 10),
		WatchCacheTTL:                 secondsEnv("BOT_WATCH_CACHE_TTL_SECONDS", 3),
		PollTimeout:                   secondsEnv("BOT_POLL_TIMEOUT", 50),
		RequestTimeout:                secondsEnv("BOT_REQUEST_TIMEOUT", 70),
		TronPollInterval:              secondsEnv("TRON_POLL_INTERVAL_SECONDS", 1),
		TronBackfillEvery:             secondsEnv("TRON_ADDRESS_BACKFILL_SECONDS", 60),
		TronLookbackMinutes:           intEnv("TRON_INITIAL_LOOKBACK_MINUTES", 15),
		TronAPIBase:                   strings.TrimRight(envAny([]string{"TRONSCAN_API_BASE", "TRONGRID_API_BASE"}, "https://apilist.tronscanapi.com/api"), "/"),
		TronAPIKey:                    strings.TrimSpace(os.Getenv("TRONGRID_API_KEY")),
		USDTContract:                  env("TRON_USDT_CONTRACT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"),
		TronGlobalPages:               intEnv("TRONSCAN_GLOBAL_SCAN_PAGES", 1),
		TronAddressPages:              intEnv("TRON_ADDRESS_SCAN_PAGES", 3),
		TronAddressScanConcurrency:    intEnv("TRON_ADDRESS_SCAN_CONCURRENCY", 8),
		ChainWatcherURL:               strings.TrimRight(strings.TrimSpace(os.Getenv("CHAIN_WATCHER_URL")), "/"),
		ChainWatcherBotID:             strings.TrimSpace(os.Getenv("CHAIN_WATCHER_BOT_ID")),
		ChainWatcherSecret:            strings.TrimSpace(os.Getenv("CHAIN_WATCHER_SECRET")),
		ChainWatcherPollInterval:      secondsEnv("CHAIN_WATCHER_POLL_SECONDS", 1),
		ChainWatcherBatchSize:         intEnv("CHAIN_WATCHER_BATCH_SIZE", 50),
		ChainWatcherEmergencyFallback: boolEnv("CHAIN_WATCHER_EMERGENCY_FALLBACK", false),
		BotWatcherHealthInterval:      secondsEnv("BOT_WATCHER_HEALTH_INTERVAL_SECONDS", 1),
		BotWatcherFailThreshold:       intEnv("BOT_WATCHER_FAIL_THRESHOLD", 2),
		BotWatcherClaimTimeout:        millisEnv("BOT_WATCHER_CLAIM_TIMEOUT_MS", 2000),
		BotFallbackPollInterval:       secondsEnv("BOT_FALLBACK_POLL_SECONDS", 1),
		BotFallbackMaxActive:          secondsEnv("BOT_FALLBACK_MAX_ACTIVE_SECONDS", 600),
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
	if cfg.TronAddressPages < 1 {
		cfg.TronAddressPages = 1
	}
	if cfg.TronAddressScanConcurrency < 1 {
		cfg.TronAddressScanConcurrency = 1
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
	if cfg.BotFallbackMaxActive <= 0 {
		cfg.BotFallbackMaxActive = 10 * time.Minute
	}
	if cfg.AddressWatchFreeLimit < 0 {
		cfg.AddressWatchFreeLimit = 0
	}
	return cfg, nil
}

func (cfg Config) ChainWatcherEnabled() bool {
	return cfg.ChainWatcherURL != "" && cfg.ChainWatcherBotID != "" && cfg.ChainWatcherSecret != ""
}

func (cfg Config) LocalAddressWatcherEnabled() bool {
	return !cfg.ChainWatcherEnabled() || cfg.ChainWatcherEmergencyFallback
}

type ChainWatcherConfig struct {
	DatabaseURL        string
	ListenAddr         string
	Timezone           string
	RequestTimeout     time.Duration
	TronAPIBase        string
	TronAPIKey         string
	USDTContract       string
	PollInterval       time.Duration
	GlobalPages        int
	AddressInterval    time.Duration
	AddressPages       int
	AddressConcurrency int
	AddressMaxPerTick  int
	RequestInterval    time.Duration
	Lookback           time.Duration
	BotCredentials     map[string]string
	ClaimLease         time.Duration
	DeliveryRetryEvery time.Duration
}

func LoadChainWatcher() (ChainWatcherConfig, error) {
	cfg := ChainWatcherConfig{
		DatabaseURL:        envAny([]string{"CHAIN_WATCHER_DATABASE_URL", "DATABASE_URL", "POSTGRES_DSN"}, "postgres://ledger:ledger@127.0.0.1:5432/ledger_bot?sslmode=disable"),
		ListenAddr:         env("CHAIN_WATCHER_ADDR", ":8090"),
		Timezone:           env("BOT_TIMEZONE", "Asia/Shanghai"),
		RequestTimeout:     secondsEnv("BOT_REQUEST_TIMEOUT", 70),
		TronAPIBase:        strings.TrimRight(envAny([]string{"CHAIN_WATCHER_TRONSCAN_API_BASE", "TRONSCAN_API_BASE", "TRONGRID_API_BASE"}, "https://apilist.tronscanapi.com/api"), "/"),
		TronAPIKey:         strings.TrimSpace(envAny([]string{"CHAIN_WATCHER_TRON_API_KEY", "TRONGRID_API_KEY"}, "")),
		USDTContract:       env("TRON_USDT_CONTRACT", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"),
		PollInterval:       secondsEnv("CHAIN_WATCHER_SOURCE_POLL_SECONDS", 1),
		GlobalPages:        intEnv("CHAIN_WATCHER_GLOBAL_SCAN_PAGES", intEnv("TRONSCAN_GLOBAL_SCAN_PAGES", 3)),
		AddressInterval:    secondsEnv("CHAIN_WATCHER_ADDRESS_SCAN_INTERVAL_SECONDS", 30),
		AddressPages:       intEnv("CHAIN_WATCHER_ADDRESS_SCAN_PAGES", 1),
		AddressConcurrency: intEnv("CHAIN_WATCHER_ADDRESS_SCAN_CONCURRENCY", 1),
		AddressMaxPerTick:  intEnv("CHAIN_WATCHER_ADDRESS_SCAN_MAX_PER_TICK", 1),
		RequestInterval:    millisEnv("CHAIN_WATCHER_TRON_REQUEST_INTERVAL_MS", 250),
		Lookback:           secondsEnv("CHAIN_WATCHER_LOOKBACK_SECONDS", 600),
		BotCredentials:     parseBotCredentials(os.Getenv("CHAIN_WATCHER_BOTS")),
		ClaimLease:         secondsEnv("CHAIN_WATCHER_CLAIM_LEASE_SECONDS", 30),
		DeliveryRetryEvery: secondsEnv("CHAIN_WATCHER_DELIVERY_RETRY_SECONDS", 2),
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return cfg, errors.New("DATABASE_URL is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.GlobalPages < 1 {
		cfg.GlobalPages = 1
	}
	if cfg.AddressInterval <= 0 {
		cfg.AddressInterval = 30 * time.Second
	}
	if cfg.AddressPages < 1 {
		cfg.AddressPages = 1
	}
	if cfg.AddressConcurrency < 1 {
		cfg.AddressConcurrency = 1
	}
	if cfg.AddressMaxPerTick < 1 {
		cfg.AddressMaxPerTick = 1
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 10 * time.Minute
	}
	if cfg.RequestInterval < 0 {
		cfg.RequestInterval = 0
	}
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
