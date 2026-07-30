package providersecret

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

const (
	testProviderID  = "45000000-0000-4000-8000-000000000001"
	testReferenceID = "65000000-0000-4000-8000-000000000001"
)

func TestLocalCipherRoundTripUsesRandomNonceAndBoundAAD(t *testing.T) {
	t.Parallel()
	randomBytes := append(bytes.Repeat([]byte{0x11}, 12), bytes.Repeat([]byte{0x22}, 12)...)
	local, err := NewLocalCipher("local-v2", bytes.Repeat([]byte{0x42}, 32), nil, bytes.NewReader(randomBytes))
	if err != nil {
		t.Fatalf("NewLocalCipher() error = %v", err)
	}
	binding := Binding{ReferenceID: testReferenceID, ProviderID: testProviderID, Name: "primary-key"}
	plaintext := []byte("synthetic-provider-credential")
	first, err := local.Encrypt(context.Background(), binding, plaintext)
	if err != nil {
		t.Fatalf("Encrypt(first) error = %v", err)
	}
	second, err := local.Encrypt(context.Background(), binding, plaintext)
	if err != nil {
		t.Fatalf("Encrypt(second) error = %v", err)
	}
	if bytes.Equal(first.Nonce, second.Nonce) || bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Fatal("two envelopes reused nonce or ciphertext")
	}
	if bytes.Contains(first.Ciphertext, plaintext) || bytes.Contains(second.Ciphertext, plaintext) {
		t.Fatal("ciphertext contains plaintext")
	}
	resolved, err := local.Decrypt(context.Background(), binding, first)
	if err != nil {
		t.Fatalf("Decrypt() error = %v", err)
	}
	if !bytes.Equal(resolved, plaintext) {
		t.Fatalf("resolved plaintext = %q", resolved)
	}
	clear(resolved)

	altered := binding
	altered.Name = "secondary-key"
	if _, err := local.Decrypt(context.Background(), altered, first); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("Decrypt(altered binding) error = %v, want ErrDecryptionFailed", err)
	}
}

func TestLocalCipherRetainedVersionDecryptsOldEnvelope(t *testing.T) {
	t.Parallel()
	oldKey := bytes.Repeat([]byte{0x31}, 32)
	currentKey := bytes.Repeat([]byte{0x32}, 32)
	binding := Binding{ReferenceID: testReferenceID, ProviderID: testProviderID, Name: "rotated-key"}
	oldCipher, err := NewLocalCipher("local-v1", oldKey, nil, bytes.NewReader(bytes.Repeat([]byte{0x41}, 12)))
	if err != nil {
		t.Fatalf("NewLocalCipher(old) error = %v", err)
	}
	envelope, err := oldCipher.Encrypt(context.Background(), binding, []byte("old-envelope-value"))
	if err != nil {
		t.Fatalf("Encrypt(old) error = %v", err)
	}
	rotated, err := NewLocalCipher(
		"local-v2",
		currentKey,
		map[string][]byte{"local-v1": oldKey},
		bytes.NewReader(bytes.Repeat([]byte{0x42}, 12)),
	)
	if err != nil {
		t.Fatalf("NewLocalCipher(rotated) error = %v", err)
	}
	plaintext, err := rotated.Decrypt(context.Background(), binding, envelope)
	if err != nil || string(plaintext) != "old-envelope-value" {
		t.Fatalf("Decrypt(retained) plaintext/error = %q/%v", plaintext, err)
	}
	clear(plaintext)
}

func TestLocalCipherRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		version  string
		key      []byte
		retained map[string][]byte
	}{
		{name: "bad version", version: "bad version", key: bytes.Repeat([]byte{1}, 32)},
		{name: "short key", version: "local-v1", key: bytes.Repeat([]byte{1}, 31)},
		{name: "duplicate version", version: "local-v1", key: bytes.Repeat([]byte{1}, 32), retained: map[string][]byte{"local-v1": bytes.Repeat([]byte{2}, 32)}},
	}
	for _, test := range tests {
		if _, err := NewLocalCipher(test.version, test.key, test.retained, bytes.NewReader(make([]byte, 12))); err == nil {
			t.Errorf("%s: NewLocalCipher() error = nil", test.name)
		}
	}
}
