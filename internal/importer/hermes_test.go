package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shero4/toolmux/internal/discovery"
)

func TestReadProfileSkipsToolmuxItself(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := []byte("mcp_servers:\n  stripe:\n    url: https://example.test/mcp\n  toolmux:\n    url: http://localhost:8080/mcp\n")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := readProfile(discoveryCandidate(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(profile.Servers) != 1 || profile.Servers[0].Name != "stripe" {
		t.Fatalf("unexpected imported servers: %#v", profile.Servers)
	}
}

func discoveryCandidate(path string) discovery.Candidate {
	return discovery.Candidate{ID: "test", Runtime: "hermes", Profile: "default", ConfigPath: path}
}

func TestWriteToolmuxServerReplacesConfigAndKeepsBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte("mcp_servers:\n  existing:\n    url: https://example.test/mcp\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeToolmuxServer(path, "http://localhost:8080/mcp", "tmx_test"); err != nil {
		t.Fatal(err)
	}
	configured, err := hasToolmuxServer(path, "http://localhost:8080/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if !configured {
		t.Fatal("Toolmux entry was not written")
	}
	backup, err := os.ReadFile(path + ".toolmux.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(original) {
		t.Fatal("backup does not match the original configuration")
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "existing:") {
		t.Fatal("existing MCP configuration was not preserved")
	}
	if err := removeToolmuxServer(path); err != nil {
		t.Fatal(err)
	}
	configured, err = hasToolmuxServer(path, "http://localhost:8080/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if configured {
		t.Fatal("Toolmux entry was not removed")
	}
	updated, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "existing:") {
		t.Fatal("existing MCP configuration was removed")
	}
}
