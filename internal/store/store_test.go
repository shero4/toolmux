package store

import (
	"strings"
	"testing"
)

func TestSlug(t *testing.T) {
	tests := map[string]string{"Automation Agent": "automation-agent", "  GitHub / Prod ": "github-prod", "---": ""}
	for input, want := range tests {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q)=%q, want %q", input, got, want)
		}
	}
}

func TestExposedToolNameIsStableAndBounded(t *testing.T) {
	upstream := strings.Repeat("very_long_tool_name_", 12)
	first := exposedToolName("primary-account", upstream)
	second := exposedToolName("primary-account", upstream)
	if first != second {
		t.Fatal("exposed name is not stable")
	}
	if len(first) > 128 {
		t.Fatalf("exposed name is %d characters", len(first))
	}
	if first == exposedToolName("secondary-account", upstream) {
		t.Fatal("connection prefix was not preserved")
	}
}
