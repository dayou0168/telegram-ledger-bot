package storage

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestKeyCipherRoundTripAndNoPlaintext(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	cipher, err := newKeyCipher(encoded)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := cipher.encrypt("private-api-key")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), "private-api-key") {
		t.Fatal("ciphertext contains plaintext")
	}
	plain, err := cipher.decrypt(ciphertext)
	if err != nil || plain != "private-api-key" {
		t.Fatalf("round trip = %q/%v", plain, err)
	}
}

func TestKeyCipherRequires32Bytes(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("short"))
	if _, err := newKeyCipher(encoded); err == nil {
		t.Fatal("short encryption key accepted")
	}
}
