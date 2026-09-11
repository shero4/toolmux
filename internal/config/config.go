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
	fileValues := readEnvironmentFile()
	get := func(name, legacyName, fallback string) string {
		return value(fileValues, name, legacyName, fallback)
	}
	cfg := Config{
		Addr:           get("TOOLMUX_ADDR", "SENTINEL_ADDR", ":8080"),
		BaseURL:        strings.TrimRight(get("TOOLMUX_BASE_URL", "SENTINEL_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:    get("TOOLMUX_DATABASE_URL", "SENTINEL_DATABASE_URL", "postgres://toolmux:toolmux@127.0.0.1:5432/toolmux?sslmode=disable"),
		DiscoveryRoots: splitPaths(get("TOOLMUX_DISCOVERY_ROOTS", "SENTINEL_DISCOVERY_ROOTS", "")),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("TOOLMUX_DATABASE_URL is required")
	}
	key, err := base64.StdEncoding.DecodeString(get("TOOLMUX_MASTER_KEY", "SENTINEL_MASTER_KEY", ""))
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

func value(fileValues map[string]string, name, legacyName, fallback string) string {
	for _, candidate := range []string{name, legacyName} {
		if value := os.Getenv(candidate); value != "" {
			return value
		}
		if value := fileValues[candidate]; value != "" {
			return value
		}
	}
	return fallback
}

func readEnvironmentFile() map[string]string {
	path := os.Getenv("TOOLMUX_ENV_FILE")
	if path == "" {
		path = ".env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		values[name] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values
}
