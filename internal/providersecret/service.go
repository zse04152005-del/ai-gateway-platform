package providersecret

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"
)

// Manager coordinates reference creation and resolution without logging or serializing plaintext.
type Manager struct {
	store    Store
	cipher   EnvelopeCipher
	external map[Backend]ExternalResolver
	random   io.Reader
	clock    func() time.Time
}

// NewManager validates and copies its resolver registry.
func NewManager(
	store Store,
	cipher EnvelopeCipher,
	external map[Backend]ExternalResolver,
	randomSource io.Reader,
	clock func() time.Time,
) (*Manager, error) {
	if store == nil {
		return nil, errors.New("provider secret store must not be nil")
	}
	if cipher == nil {
		return nil, errors.New("provider secret envelope cipher must not be nil")
	}
	if randomSource == nil {
		return nil, errors.New("provider secret random source must not be nil")
	}
	if clock == nil {
		return nil, errors.New("provider secret clock must not be nil")
	}
	registry := make(map[Backend]ExternalResolver, len(external))
	for backend, resolver := range external {
		if (backend != BackendVault && backend != BackendKMS) || resolver == nil {
			return nil, errors.New("provider secret external resolver registry is invalid")
		}
		registry[backend] = resolver
	}
	return &Manager{store: store, cipher: cipher, external: registry, random: randomSource, clock: clock}, nil
}

// CreateLocal encrypts plaintext before constructing the persistence record.
func (manager *Manager) CreateLocal(ctx context.Context, command CreateLocalCommand) (Metadata, error) {
	if err := validateCreateLocal(command); err != nil {
		return Metadata{}, err
	}
	id, err := newUUID(manager.random)
	if err != nil {
		return Metadata{}, err
	}
	binding := Binding{ReferenceID: id, ProviderID: command.ProviderID, Name: command.Name}
	plaintext := append([]byte(nil), command.Plaintext...)
	defer clear(plaintext)
	envelope, err := manager.cipher.Encrypt(ctx, binding, plaintext)
	if err != nil {
		return Metadata{}, errors.New("encrypt provider secret failed")
	}
	now := manager.clock().UTC()
	keyVersion := envelope.KeyVersion
	record, err := manager.store.Create(ctx, Record{
		ID: id, ProviderID: command.ProviderID, Name: command.Name,
		Backend: BackendLocalEnvelope, Ciphertext: append([]byte(nil), envelope.Ciphertext...),
		Nonce: append([]byte(nil), envelope.Nonce...), KeyVersion: &keyVersion,
		Status: StatusActive, Version: 1, CreatedAt: now, CreatedBy: command.Actor,
		UpdatedAt: now, UpdatedBy: command.Actor,
	})
	clear(envelope.Ciphertext)
	clear(envelope.Nonce)
	if err != nil {
		return Metadata{}, fmt.Errorf("persist provider secret reference: %w", err)
	}
	return record.Metadata(), nil
}

// RegisterExternal persists an internal Vault/KMS locator without resolving or storing plaintext.
func (manager *Manager) RegisterExternal(ctx context.Context, command RegisterExternalCommand) (Metadata, error) {
	if err := validateRegisterExternal(command); err != nil {
		return Metadata{}, err
	}
	id, err := newUUID(manager.random)
	if err != nil {
		return Metadata{}, err
	}
	now := manager.clock().UTC()
	locator := command.Locator
	record, err := manager.store.Create(ctx, Record{
		ID: id, ProviderID: command.ProviderID, Name: command.Name,
		Backend: command.Backend, Locator: &locator, Status: StatusActive, Version: 1,
		CreatedAt: now, CreatedBy: command.Actor, UpdatedAt: now, UpdatedBy: command.Actor,
	})
	if err != nil {
		return Metadata{}, fmt.Errorf("persist provider secret reference: %w", err)
	}
	return record.Metadata(), nil
}

// Resolve returns a copied plaintext only to the provider-adapter call boundary.
// Callers must clear the returned bytes immediately after building the upstream request.
func (manager *Manager) Resolve(ctx context.Context, locator Locator) ([]byte, error) {
	if err := validateLocator(locator); err != nil {
		return nil, err
	}
	record, err := manager.store.Get(ctx, locator)
	if err != nil {
		return nil, fmt.Errorf("load provider secret reference: %w", err)
	}
	if record.Status != StatusActive {
		return nil, ErrDisabled
	}
	binding := Binding{ReferenceID: record.ID, ProviderID: record.ProviderID, Name: record.Name}
	switch record.Backend {
	case BackendLocalEnvelope:
		if record.KeyVersion == nil {
			return nil, ErrDecryptionFailed
		}
		plaintext, err := manager.cipher.Decrypt(ctx, binding, Envelope{
			Ciphertext: record.Ciphertext, Nonce: record.Nonce, KeyVersion: *record.KeyVersion,
		})
		if err != nil {
			return nil, ErrDecryptionFailed
		}
		return plaintext, nil
	case BackendVault, BackendKMS:
		resolver := manager.external[record.Backend]
		if resolver == nil || record.Locator == nil {
			return nil, ErrBackendUnavailable
		}
		plaintext, err := resolver.Resolve(ctx, *record.Locator)
		if err != nil || len(plaintext) < 1 || len(plaintext) > maximumSecretBytes {
			clear(plaintext)
			return nil, ErrBackendUnavailable
		}
		copyOfPlaintext := append([]byte(nil), plaintext...)
		clear(plaintext)
		return copyOfPlaintext, nil
	default:
		return nil, ErrBackendUnavailable
	}
}

func newUUID(reader io.Reader) (string, error) {
	if reader == nil {
		reader = rand.Reader
	}
	raw := make([]byte, 16)
	if err := readRandom(reader, raw); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16],
	), nil
}
