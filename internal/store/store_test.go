package store

import "testing"

func TestSlug(t *testing.T) {
	tests := map[string]string{"Automation Agent": "automation-agent", "  GitHub / Prod ": "github-prod", "---": ""}
	for input, want := range tests {
		if got := Slug(input); got != want {
			t.Errorf("Slug(%q)=%q, want %q", input, got, want)
		}
	}
}
