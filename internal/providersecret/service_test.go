package providersecret

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestManagerEncryptsBeforePersistenceAndReturnsSafeMetadata(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 30, 14, 0, 0, 0, time.UTC)
	store := &stubSecretStore{}
	local := mustLocalCipher(t, 0x51)
	manager, err := NewManager(store, local, nil, bytes.NewReader(bytes.Repeat([]byte{0x61}, 16)), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	plaintext := []byte("synthetic-provider-credential")
	metadata, err := manager.CreateLocal(context.Background(), CreateLocalCommand{
		ProviderID: testProviderID, Name: "primary-key", Plaintext: plaintext, Actor: "unit:provider-secret",
	})
	if err != nil {
		t.Fatalf("CreateLocal() error = %v", err)
	}
	if metadata.ID == "" || metadata.ProviderID != testProviderID || metadata.Backend != BackendLocalEnvelope {
		t.Fatalf("metadata = %+v", metadata)
	}
	if bytes.Contains(store.created.Ciphertext, plaintext) || len(store.created.Nonce) != 12 || store.created.KeyVersion == nil {
		t.Fatalf("stored envelope is invalid or contains plaintext")
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal(metadata) error = %v", err)
	}
	encodedRecord, err := json.Marshal(store.created)
	if err != nil {
		t.Fatalf("json.Marshal(record) error = %v", err)
	}
	for _, encoded := range [][]byte{encodedMetadata, encodedRecord} {
		for _, forbidden := range []string{"synthetic-provider-credential", "ciphertext", "nonce", "key_version", "locator"} {
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Fatalf("serialized value contains %q: %s", forbidden, encoded)
			}
		}
	}
	resolved, err := manager.Resolve(context.Background(), Locator{ProviderID: testProviderID, ID: metadata.ID})
	if err != nil || !bytes.Equal(resolved, plaintext) {
		t.Fatalf("Resolve() value/error = %q/%v", resolved, err)
	}
	clear(resolved)
}

func TestManagerUsesExternalResolverWithoutExposingLocatorErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 30, 14, 0, 0, 0, time.UTC)
	store := &stubSecretStore{}
	resolver := &stubExternalResolver{plaintext: []byte("external-provider-value")}
	manager, err := NewManager(
		store,
		mustLocalCipher(t, 0x52),
		map[Backend]ExternalResolver{BackendVault: resolver},
		bytes.NewReader(bytes.Repeat([]byte{0x62}, 32)),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	locatorValue := "vault://provider-secrets/team-a#primary"
	metadata, err := manager.RegisterExternal(context.Background(), RegisterExternalCommand{
		ProviderID: testProviderID, Name: "vault-primary", Backend: BackendVault,
		Locator: locatorValue, Actor: "unit:vault",
	})
	if err != nil {
		t.Fatalf("RegisterExternal() error = %v", err)
	}
	resolved, err := manager.Resolve(context.Background(), Locator{ProviderID: testProviderID, ID: metadata.ID})
	if err != nil || string(resolved) != "external-provider-value" || resolver.locator != locatorValue {
		t.Fatalf("Resolve(vault) value/error/locator = %q/%v/%q", resolved, err, resolver.locator)
	}
	clear(resolved)

	resolver.err = errors.New("backend rejected " + locatorValue + " with external-provider-value")
	if _, err := manager.Resolve(context.Background(), Locator{ProviderID: testProviderID, ID: metadata.ID}); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("Resolve(failed vault) error = %v", err)
	} else if strings.Contains(err.Error(), locatorValue) || strings.Contains(err.Error(), "external-provider-value") {
		t.Fatalf("safe error leaked locator or plaintext: %v", err)
	}
}

func TestManagerFailsClosedForDisabledOrSwappedReferences(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 30, 14, 0, 0, 0, time.UTC)
	store := &stubSecretStore{}
	manager, err := NewManager(
		store,
		mustLocalCipher(t, 0x53),
		nil,
		bytes.NewReader(bytes.Repeat([]byte{0x63}, 16)),
		func() time.Time { return now },
	)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	metadata, err := manager.CreateLocal(context.Background(), CreateLocalCommand{
		ProviderID: testProviderID, Name: "disabled-key", Plaintext: []byte("disabled-value"), Actor: "unit:disable",
	})
	if err != nil {
		t.Fatalf("CreateLocal() error = %v", err)
	}
	store.created.Status = StatusDisabled
	if _, err := manager.Resolve(context.Background(), Locator{ProviderID: testProviderID, ID: metadata.ID}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Resolve(disabled) error = %v", err)
	}
	store.created.Status = StatusActive
	store.created.Name = "swapped-name"
	if _, err := manager.Resolve(context.Background(), Locator{ProviderID: testProviderID, ID: metadata.ID}); !errors.Is(err, ErrDecryptionFailed) {
		t.Fatalf("Resolve(swapped AAD) error = %v", err)
	}
}

type stubSecretStore struct {
	created Record
	err     error
}

func (store *stubSecretStore) Create(_ context.Context, record Record) (Record, error) {
	if store.err != nil {
		return Record{}, store.err
	}
	store.created = cloneSecretRecord(record)
	return cloneSecretRecord(record), nil
}

func (store *stubSecretStore) Get(_ context.Context, locator Locator) (Record, error) {
	if store.err != nil {
		return Record{}, store.err
	}
	if store.created.ID != locator.ID || store.created.ProviderID != locator.ProviderID {
		return Record{}, ErrNotFound
	}
	return cloneSecretRecord(store.created), nil
}

type stubExternalResolver struct {
	plaintext []byte
	err       error
	locator   string
}

func (resolver *stubExternalResolver) Resolve(_ context.Context, locator string) ([]byte, error) {
	resolver.locator = locator
	if resolver.err != nil {
		return nil, resolver.err
	}
	return append([]byte(nil), resolver.plaintext...), nil
}

func mustLocalCipher(t *testing.T, nonceByte byte) *LocalCipher {
	t.Helper()
	local, err := NewLocalCipher(
		"local-v1",
		bytes.Repeat([]byte{0x41}, 32),
		nil,
		bytes.NewReader(bytes.Repeat([]byte{nonceByte}, 64)),
	)
	if err != nil {
		t.Fatalf("NewLocalCipher() error = %v", err)
	}
	return local
}

func cloneSecretRecord(record Record) Record {
	record.Ciphertext = append([]byte(nil), record.Ciphertext...)
	record.Nonce = append([]byte(nil), record.Nonce...)
	if record.Locator != nil {
		value := *record.Locator
		record.Locator = &value
	}
	if record.KeyVersion != nil {
		value := *record.KeyVersion
		record.KeyVersion = &value
	}
	return record
}
