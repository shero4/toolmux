// Package config loads runtime settings from the environment, with a local
// .env file as a fallback for development.
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
	get := func(name, fallback string) string {
		if value := os.Getenv(name); value != "" {
			return value
		}
		if value := fileValues[name]; value != "" {
			return value
		}
		return fallback
	}
	cfg := Config{
		Addr:           get("TOOLMUX_ADDR", "127.0.0.1:8080"),
		BaseURL:        strings.TrimRight(get("TOOLMUX_BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:    get("TOOLMUX_DATABASE_URL", "postgres://toolmux:toolmux@127.0.0.1:5432/toolmux?sslmode=disable"),
		DiscoveryRoots: splitPaths(get("TOOLMUX_DISCOVERY_ROOTS", "")),
	}
	key, err := base64.StdEncoding.DecodeString(get("TOOLMUX_MASTER_KEY", ""))
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

// readEnvironmentFile parses KEY=value lines from TOOLMUX_ENV_FILE or ./.env.
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
