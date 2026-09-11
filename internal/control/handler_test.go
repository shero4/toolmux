package control

import (
	"strings"
	"testing"
)

func TestAdminTokenIsStableAndSeparated(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	first := Token(key)
	if first != Token(key) {
		t.Fatal("admin token is not stable")
	}
	if !strings.HasPrefix(first, "tmx_admin_") {
		t.Fatalf("unexpected token prefix: %q", first)
	}
	other := []byte("11234567890123456789012345678901")
	if first == Token(other) {
		t.Fatal("different installation keys produced the same token")
	}
}
