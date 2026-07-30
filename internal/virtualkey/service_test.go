package virtualkey

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testTenantID  = "10000000-0000-4000-8000-000000000001"
	testProjectID = "20000000-0000-4000-8000-000000000001"
	testRecordID  = "30000000-0000-4000-8000-000000000001"
)

func TestManagerCreatePersistsOnlyDigestAndReturnsPlaintextOnce(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	store := &stubStore{}
	digester := mustTestDigester(t)
	manager := mustTestManager(t, store, digester, now)
	expiresAt := now.Add(time.Hour)
	models := []string{"chat.default"}
	rpm := int64(120)

	issued, err := manager.Create(context.Background(), CreateCommand{
		TenantID:      testTenantID,
		ProjectID:     testProjectID,
		Mode:          "live",
		ExpiresAt:     &expiresAt,
		AllowedModels: &models,
		Limits:        &Limits{RPM: &rpm},
		Actor:         "unit:test",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !strings.HasPrefix(issued.Credential, issued.Metadata.Prefix+".") {
		t.Fatalf("credential does not contain its safe prefix")
	}
	parts := strings.Split(issued.Credential, ".")
	if len(parts) != 2 {
		t.Fatalf("credential part count = %d, want 2", len(parts))
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(secret) != secretRandomBytes {
		t.Fatalf("credential secret length/error = %d/%v", len(secret), err)
	}
	if len(store.created.SecretHash) != digestSize {
		t.Fatalf("stored digest length = %d, want %d", len(store.created.SecretHash), digestSize)
	}
	if bytes.Contains(store.created.SecretHash, []byte(parts[1])) || bytes.Equal(store.created.SecretHash, secret) {
		t.Fatal("store received recoverable plaintext instead of a keyed digest")
	}
	if store.created.HashKeyVersion != "unit-v1" {
		t.Fatalf("digest version = %q", store.created.HashKeyVersion)
	}

	metadata, err := manager.Get(context.Background(), Locator{
		TenantID: testTenantID, ProjectID: testProjectID, ID: issued.Metadata.ID,
	})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal(metadata) error = %v", err)
	}
	for _, forbidden := range []string{"credential", "secret_hash", parts[1]} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("metadata contains forbidden value %q: %s", forbidden, encoded)
		}
	}
	recordJSON, err := json.Marshal(store.created)
	if err != nil {
		t.Fatalf("json.Marshal(record) error = %v", err)
	}
	if bytes.Contains(recordJSON, store.created.SecretHash) || bytes.Contains(recordJSON, []byte("SecretHash")) {
		t.Fatalf("internal record JSON exposes digest: %s", recordJSON)
	}
}

func TestManagerRetriesIdentityCollisionWithoutReturningFailedPlaintext(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	store := &stubStore{createErrors: []error{ErrCollision, nil}}
	manager := mustTestManager(t, store, mustTestDigester(t), now)

	issued, err := manager.Create(context.Background(), CreateCommand{
		TenantID: testTenantID, ProjectID: testProjectID, Mode: "test", Actor: "unit:test",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if store.createCalls != 2 {
		t.Fatalf("Create() store calls = %d, want 2", store.createCalls)
	}
	if issued.Credential == "" || !strings.HasPrefix(issued.Metadata.Prefix, "agw_test_") {
		t.Fatalf("issued credential metadata = %+v", issued.Metadata)
	}
}

func TestManagerRotateAndRevoke(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	source := testRecord(now)
	store := &stubStore{current: source}
	manager := mustTestManager(t, store, mustTestDigester(t), now)
	locator := Locator{TenantID: source.TenantID, ProjectID: source.ProjectID, ID: source.ID}

	rotated, err := manager.Rotate(context.Background(), RotateCommand{
		Locator: locator, GracePeriod: 15 * time.Minute, Actor: "unit:rotate",
	})
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if rotated.Credential == "" || rotated.Metadata.RotatedFromID == nil || *rotated.Metadata.RotatedFromID != source.ID {
		t.Fatalf("rotated credential = %+v", rotated)
	}
	if !store.graceExpiresAt.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("grace deadline = %v", store.graceExpiresAt)
	}
	if store.rotateActor != "unit:rotate" {
		t.Fatalf("rotate actor = %q", store.rotateActor)
	}

	revoked, err := manager.Revoke(context.Background(), RevokeCommand{
		Locator: locator, Actor: "unit:revoke",
	})
	if err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if revoked.Status != StateRevoked || revoked.EffectiveStatus != EffectiveState(StateRevoked) {
		t.Fatalf("revoked metadata = %+v", revoked)
	}
}

func TestMetadataDerivesAbsoluteAndRotationExpiry(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := now.Add(-time.Second)
	record := testRecord(now)
	record.ExpiresAt = &expiresAt
	if got := record.Metadata(now).EffectiveStatus; got != EffectiveExpired {
		t.Fatalf("absolute effective state = %q, want %q", got, EffectiveExpired)
	}

	graceDeadline := now
	record.ExpiresAt = nil
	record.Status = StateRotating
	record.RotationGraceExpiresAt = &graceDeadline
	if got := record.Metadata(now).EffectiveStatus; got != EffectiveRotationGraceElapsed {
		t.Fatalf("rotation effective state = %q, want %q", got, EffectiveRotationGraceElapsed)
	}
}

func TestManagerValidatesCreateAndRotationBoundaries(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	manager := mustTestManager(t, &stubStore{current: testRecord(now)}, mustTestDigester(t), now)
	zero := int64(0)
	tests := []struct {
		name    string
		command CreateCommand
		field   string
	}{
		{name: "bad tenant", command: CreateCommand{TenantID: "bad", ProjectID: testProjectID, Mode: "live", Actor: "actor"}, field: "tenant_id"},
		{name: "bad mode", command: CreateCommand{TenantID: testTenantID, ProjectID: testProjectID, Mode: "stage", Actor: "actor"}, field: "mode"},
		{name: "missing actor", command: CreateCommand{TenantID: testTenantID, ProjectID: testProjectID, Mode: "live"}, field: "actor"},
		{name: "non-positive limit", command: CreateCommand{TenantID: testTenantID, ProjectID: testProjectID, Mode: "live", Actor: "actor", Limits: &Limits{RPM: &zero}}, field: "limits.rpm"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := manager.Create(context.Background(), test.command)
			var validationError *ValidationError
			if !errors.As(err, &validationError) || validationError.Field != test.field {
				t.Fatalf("Create() error = %v, want validation field %q", err, test.field)
			}
		})
	}

	_, err := manager.Rotate(context.Background(), RotateCommand{
		Locator: Locator{TenantID: testTenantID, ProjectID: testProjectID, ID: testRecordID},
		Actor:   "actor",
	})
	var validationError *ValidationError
	if !errors.As(err, &validationError) || validationError.Field != "grace_period_seconds" {
		t.Fatalf("Rotate() error = %v, want grace validation", err)
	}
}

func TestHMACDigesterBindsPrefixAndDoesNotRetainCallerKey(t *testing.T) {
	key := bytes.Repeat([]byte{0x41}, digestSize)
	digester, err := NewHMACDigester("unit-v1", key)
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	first := digester.Digest("agw_live_00000001", []byte("secret"))
	second := digester.Digest("agw_live_00000002", []byte("secret"))
	if hmac.Equal(first, second) {
		t.Fatal("digest must bind the public prefix")
	}
	clear(key)
	third := digester.Digest("agw_live_00000001", []byte("secret"))
	if !hmac.Equal(first, third) {
		t.Fatal("digester retained caller-owned key slice")
	}
}

type stubStore struct {
	created        Record
	current        Record
	createErrors   []error
	createCalls    int
	graceExpiresAt time.Time
	rotateActor    string
}

func (store *stubStore) Create(_ context.Context, record Record) (Record, error) {
	store.createCalls++
	if len(store.createErrors) >= store.createCalls && store.createErrors[store.createCalls-1] != nil {
		return Record{}, store.createErrors[store.createCalls-1]
	}
	store.created = record
	store.current = record
	return record, nil
}

func (store *stubStore) Get(_ context.Context, _ Locator) (Record, error) {
	if store.current.ID == "" {
		return Record{}, ErrNotFound
	}
	return store.current, nil
}

func (store *stubStore) Rotate(
	_ context.Context,
	_ Locator,
	replacement Replacement,
	graceExpiresAt time.Time,
	actor string,
	now time.Time,
) (Record, error) {
	store.graceExpiresAt = graceExpiresAt
	store.rotateActor = actor
	sourceID := store.current.ID
	rotated := Record{
		ID: replacement.ID, TenantID: store.current.TenantID, ProjectID: store.current.ProjectID,
		Prefix: replacement.Prefix, SecretHash: replacement.SecretHash, HashKeyVersion: replacement.HashKeyVersion,
		Status: StateActive, ExpiresAt: cloneTime(store.current.ExpiresAt), AllowedModels: cloneStrings(store.current.AllowedModels),
		Limits: cloneLimits(store.current.Limits), RotatedFromID: &sourceID, Version: 1,
		CreatedAt: now, CreatedBy: actor, UpdatedAt: now, UpdatedBy: actor,
	}
	store.current = rotated
	return rotated, nil
}

func (store *stubStore) Revoke(_ context.Context, _ Locator, actor string, now time.Time) (Record, error) {
	store.current.Status = StateRevoked
	store.current.RevokedAt = &now
	store.current.RevokedBy = &actor
	store.current.UpdatedAt = now
	store.current.UpdatedBy = actor
	store.current.Version++
	return store.current, nil
}

func mustTestDigester(t *testing.T) *HMACDigester {
	t.Helper()
	digester, err := NewHMACDigester("unit-v1", bytes.Repeat([]byte{0x7a}, digestSize))
	if err != nil {
		t.Fatalf("NewHMACDigester() error = %v", err)
	}
	return digester
}

func mustTestManager(t *testing.T, store Store, digester Digester, now time.Time) *Manager {
	t.Helper()
	randomBytes := bytes.Repeat([]byte{0x2a}, 1024)
	manager, err := NewManager(store, digester, bytes.NewReader(randomBytes), func() time.Time { return now })
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager
}

func testRecord(now time.Time) Record {
	return Record{
		ID: testRecordID, TenantID: testTenantID, ProjectID: testProjectID,
		Prefix: "agw_live_00000001", SecretHash: bytes.Repeat([]byte{0x11}, digestSize), HashKeyVersion: "unit-v1",
		Status: StateActive, Version: 1, CreatedAt: now.Add(-time.Hour), CreatedBy: "unit:create",
		UpdatedAt: now.Add(-time.Hour), UpdatedBy: "unit:create",
	}
}
