package secretbox

import (
	"bytes"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	box, err := New(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := box.Seal([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := box.Open(ciphertext, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if string(plaintext) != "secret" {
		t.Fatalf("got %q", plaintext)
	}
}
func TestWrongKeyFails(t *testing.T) {
	one, _ := New(bytes.Repeat([]byte{1}, 32))
	two, _ := New(bytes.Repeat([]byte{2}, 32))
	ciphertext, nonce, _ := one.Seal([]byte("secret"))
	if _, err := two.Open(ciphertext, nonce); err == nil {
		t.Fatal("expected decryption failure")
	}
}
