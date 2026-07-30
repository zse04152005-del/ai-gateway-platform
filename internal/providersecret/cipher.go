package providersecret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"io"
)

// LocalCipher provides versioned AES-256-GCM envelopes for local development.
// Production must inject Vault/KMS adapters instead of configuring this cipher.
type LocalCipher struct {
	currentVersion string
	keys           map[string][]byte
	random         io.Reader
}

// NewLocalCipher copies the current and retained 32-byte keys.
func NewLocalCipher(currentVersion string, currentKey []byte, retained map[string][]byte, random io.Reader) (*LocalCipher, error) {
	if !keyVersionPattern.MatchString(currentVersion) {
		return nil, errors.New("provider secret key version has an invalid format")
	}
	if len(currentKey) != 32 {
		return nil, errors.New("provider secret local envelope key must contain exactly 32 bytes")
	}
	if random == nil {
		return nil, errors.New("provider secret random source must not be nil")
	}
	keys := make(map[string][]byte, len(retained)+1)
	keys[currentVersion] = append([]byte(nil), currentKey...)
	for version, key := range retained {
		if !keyVersionPattern.MatchString(version) || len(key) != 32 || version == currentVersion {
			return nil, errors.New("provider secret retained envelope key is invalid")
		}
		keys[version] = append([]byte(nil), key...)
	}
	return &LocalCipher{currentVersion: currentVersion, keys: keys, random: random}, nil
}

// Encrypt seals a non-empty secret and authenticates its reference identity as AAD.
func (local *LocalCipher) Encrypt(ctx context.Context, binding Binding, plaintext []byte) (Envelope, error) {
	if err := ctx.Err(); err != nil {
		return Envelope{}, err
	}
	if err := validateBinding(binding); err != nil {
		return Envelope{}, err
	}
	if len(plaintext) < 1 || len(plaintext) > maximumSecretBytes {
		return Envelope{}, invalid("plaintext", "must contain 1-16384 bytes")
	}
	block, err := aes.NewCipher(local.keys[local.currentVersion])
	if err != nil {
		return Envelope{}, errors.New("create provider secret envelope cipher failed")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, errors.New("create provider secret authenticated cipher failed")
	}
	nonce := make([]byte, aead.NonceSize())
	if err := readRandom(local.random, nonce); err != nil {
		return Envelope{}, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, additionalData(binding, local.currentVersion))
	return Envelope{Ciphertext: ciphertext, Nonce: nonce, KeyVersion: local.currentVersion}, nil
}

// Decrypt opens an envelope only when version, AAD identity, nonce, and tag all match.
func (local *LocalCipher) Decrypt(ctx context.Context, binding Binding, envelope Envelope) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	key, ok := local.keys[envelope.KeyVersion]
	if !ok || len(envelope.Nonce) != 12 || len(envelope.Ciphertext) < 17 || len(envelope.Ciphertext) > 65536 {
		return nil, ErrDecryptionFailed
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, additionalData(binding, envelope.KeyVersion))
	if err != nil {
		return nil, ErrDecryptionFailed
	}
	return plaintext, nil
}

func validateBinding(binding Binding) error {
	if err := validateUUID("binding.reference_id", binding.ReferenceID); err != nil {
		return err
	}
	if err := validateUUID("binding.provider_id", binding.ProviderID); err != nil {
		return err
	}
	if !namePattern.MatchString(binding.Name) {
		return invalid("binding.name", "must be a canonical reference name")
	}
	return nil
}

func additionalData(binding Binding, version string) []byte {
	value := binding.ReferenceID + "\x00" + binding.ProviderID + "\x00" + binding.Name + "\x00" + version
	return []byte(value)
}
