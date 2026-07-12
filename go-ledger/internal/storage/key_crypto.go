package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

type keyCipher struct {
	aead cipher.AEAD
}

func newKeyCipher(encoded string) (*keyCipher, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, errors.New("CHAIN_WATCHER_KEY_ENCRYPTION_KEY is required")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode CHAIN_WATCHER_KEY_ENCRYPTION_KEY: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("CHAIN_WATCHER_KEY_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &keyCipher{aead: aead}, nil
}

func (c *keyCipher) encrypt(plain string) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("chain watcher key encryption is not configured")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (c *keyCipher) decrypt(ciphertext []byte) (string, error) {
	if c == nil || c.aead == nil {
		return "", errors.New("chain watcher key encryption is not configured")
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", errors.New("encrypted Tronscan key is truncated")
	}
	plain, err := c.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return "", errors.New("decrypt Tronscan key: authentication failed")
	}
	return string(plain), nil
}
