package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
)

type Box struct {
	aead cipher.AEAD
}

func New(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

func (b *Box) Seal(plaintext []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, fmt.Errorf("create nonce: %w", err)
	}
	return b.aead.Seal(nil, nonce, plaintext, nil), nonce, nil
}

func (b *Box) Open(ciphertext, nonce []byte) ([]byte, error) {
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt credential: %w", err)
	}
	return plaintext, nil
}
