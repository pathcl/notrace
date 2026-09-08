package config

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Tempo TempoConfig `mapstructure:"tempo"`
}

type TempoConfig struct {
	URL     string        `mapstructure:"url"`
	Token   string        `mapstructure:"token"`
	OrgID   string        `mapstructure:"org_id"`
	Timeout time.Duration `mapstructure:"timeout"`
}

func Load() (*Config, error) {
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	if cfg.Tempo.URL == "" {
		return nil, fmt.Errorf("tempo URL required — set NOTRACE_TEMPO_URL or --tempo-url")
	}
	if cfg.Tempo.Timeout <= 0 {
		cfg.Tempo.Timeout = 10 * time.Second
	}
	return &cfg, nil
}
