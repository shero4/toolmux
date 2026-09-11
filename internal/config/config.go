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
		Addr:           value("SENTINEL_ADDR", ":8080"),
		BaseURL:        strings.TrimRight(value("SENTINEL_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:    os.Getenv("SENTINEL_DATABASE_URL"),
		DiscoveryRoots: splitPaths(os.Getenv("SENTINEL_DISCOVERY_ROOTS")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("SENTINEL_DATABASE_URL is required")
	}
	key, err := base64.StdEncoding.DecodeString(os.Getenv("SENTINEL_MASTER_KEY"))
	if err != nil || len(key) != 32 {
		return Config{}, errors.New("SENTINEL_MASTER_KEY must be a base64-encoded 32-byte key")
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

func value(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
