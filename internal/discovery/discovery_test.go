package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestScanRootFindsConfiguredAgentsOnly(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".hermes", "profiles", "research", "config.yaml"), "model: test\n")
	mustWrite(t, filepath.Join(home, ".hermes", "mnemosyne", "models", "cache"), "not a profile")
	mustWrite(t, filepath.Join(home, ".openclaw", "openclaw.json"), `{"agents":{"list":[{"id":"main","name":"Personal"},{"id":"ops"}]}}`)

	got := New([]string{home}).Scan(context.Background())
	var matches []Candidate
	for _, candidate := range got {
		if candidate.Environment == "Mounted host" {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 3 {
		t.Fatalf("got %d candidates, want 3: %#v", len(matches), matches)
	}
	if matches[0].Runtime != "hermes" || matches[0].Profile != "research" {
		t.Fatalf("unexpected Hermes candidate: %#v", matches[0])
	}
}

func TestFindUsesOpaqueCandidateID(t *testing.T) {
	home := t.TempDir()
	mustWrite(t, filepath.Join(home, ".openclaw", "openclaw.json"), `{}`)
	scanner := New([]string{home})
	candidates := scanner.Scan(context.Background())
	var mounted Candidate
	for _, candidate := range candidates {
		if candidate.Environment == "Mounted host" {
			mounted = candidate
		}
	}
	if mounted.ID == "" {
		t.Fatal("expected a mounted candidate")
	}
	got, ok := scanner.Find(context.Background(), mounted.ID)
	if !ok || got.ConfigPath != mounted.ConfigPath {
		t.Fatalf("Find() = %#v, %v", got, ok)
	}
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}
