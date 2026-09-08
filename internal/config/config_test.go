package config_test

import (
	"testing"

	"github.com/pathcl/notrace/internal/config"
	"github.com/spf13/viper"
)

func TestLoadMissingURL(t *testing.T) {
	viper.Reset()
	_, err := config.Load()
	if err == nil {
		t.Fatal("expected error when tempo URL is missing")
	}
}

func TestLoadURL(t *testing.T) {
	viper.Reset()
	viper.Set("tempo.url", "http://localhost:3200")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Tempo.URL != "http://localhost:3200" {
		t.Errorf("got URL %q, want %q", cfg.Tempo.URL, "http://localhost:3200")
	}
}

func TestLoadTokenAndOrgID(t *testing.T) {
	viper.Reset()
	viper.Set("tempo.url", "http://localhost:3200")
	viper.Set("tempo.token", "secret")
	viper.Set("tempo.org_id", "tenant1")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Tempo.Token != "secret" {
		t.Errorf("got token %q, want %q", cfg.Tempo.Token, "secret")
	}
	if cfg.Tempo.OrgID != "tenant1" {
		t.Errorf("got org_id %q, want %q", cfg.Tempo.OrgID, "tenant1")
	}
}
