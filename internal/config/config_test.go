package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestLoadPrefersToolmuxEnvironment(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("TOOLMUX_DATABASE_URL", "postgres://toolmux")
	t.Setenv("SENTINEL_DATABASE_URL", "postgres://legacy")
	t.Setenv("TOOLMUX_MASTER_KEY", key)
	t.Setenv("SENTINEL_MASTER_KEY", "invalid")
	t.Setenv("TOOLMUX_BASE_URL", "https://toolmux.example/")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://toolmux" {
		t.Fatalf("database URL = %q", cfg.DatabaseURL)
	}
	if cfg.BaseURL != "https://toolmux.example" {
		t.Fatalf("base URL = %q", cfg.BaseURL)
	}
}

func TestLoadAcceptsLegacyEnvironment(t *testing.T) {
	t.Setenv("TOOLMUX_DATABASE_URL", "")
	t.Setenv("TOOLMUX_MASTER_KEY", "")
	t.Setenv("SENTINEL_DATABASE_URL", "postgres://legacy")
	t.Setenv("SENTINEL_MASTER_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32))))

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DatabaseURL != "postgres://legacy" {
		t.Fatalf("database URL = %q", cfg.DatabaseURL)
	}
}
