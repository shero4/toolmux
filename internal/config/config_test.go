package config

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadReadsEnvironment(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("TOOLMUX_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("TOOLMUX_DATABASE_URL", "postgres://toolmux")
	t.Setenv("TOOLMUX_MASTER_KEY", key)
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

func TestLoadFallsBackToEnvFile(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("# comment\nTOOLMUX_MASTER_KEY=\""+key+"\"\nTOOLMUX_ADDR=:9090\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TOOLMUX_ENV_FILE", path)
	t.Setenv("TOOLMUX_MASTER_KEY", "")
	t.Setenv("TOOLMUX_ADDR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Addr != ":9090" || len(cfg.MasterKey) != 32 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsMissingKey(t *testing.T) {
	t.Setenv("TOOLMUX_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("TOOLMUX_MASTER_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected an error without a master key")
	}
}
