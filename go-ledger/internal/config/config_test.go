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
