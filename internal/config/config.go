package config

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Addr           string
	BaseURL        string
	DatabaseURL    string
	MasterKey      []byte
	DiscoveryRoots []string
}

func Load() (Config, error) {
	cfg := Config{
		Addr:           value("TOOLMUX_ADDR", "SENTINEL_ADDR", ":8080"),
		BaseURL:        strings.TrimRight(value("TOOLMUX_BASE_URL", "SENTINEL_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:    value("TOOLMUX_DATABASE_URL", "SENTINEL_DATABASE_URL", ""),
		DiscoveryRoots: splitPaths(value("TOOLMUX_DISCOVERY_ROOTS", "SENTINEL_DISCOVERY_ROOTS", "")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("TOOLMUX_DATABASE_URL is required")
	}
	key, err := base64.StdEncoding.DecodeString(value("TOOLMUX_MASTER_KEY", "SENTINEL_MASTER_KEY", ""))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("TOOLMUX_MASTER_KEY must be a base64-encoded 32-byte key")
	}
	cfg.MasterKey = key
	return cfg, nil
}

func splitPaths(value string) []string {
	var paths []string
	for _, path := range filepath.SplitList(value) {
		if path = strings.TrimSpace(path); path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func value(name, legacyName, fallback string) string {
	for _, candidate := range []string{name, legacyName} {
		if value := os.Getenv(candidate); value != "" {
			return value
		}
	}
	return fallback
}
