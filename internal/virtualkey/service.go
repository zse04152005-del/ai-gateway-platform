package virtualkey

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	minimumExpiryLead = time.Second
	minimumGrace      = time.Second
	maximumGrace      = 24 * time.Hour
	issuanceAttempts  = 3
	prefixRandomBytes = 10
	secretRandomBytes = 32
)

var prefixEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Store atomically persists virtual credential lifecycle transitions.
type Store interface {
	Create(context.Context, Record) (Record, error)
	Get(context.Context, Locator) (Record, error)
	Rotate(context.Context, Locator, Replacement, time.Time, string, time.Time) (Record, error)
	Revoke(context.Context, Locator, string, time.Time) (Record, error)
}

// Replacement contains newly generated identity material. Store.Rotate inherits authorization and expiry under lock.
type Replacement struct {
	ID             string
	Prefix         string
	SecretHash     []byte
	HashKeyVersion string
}

// Manager coordinates validation, issuance, hashing, and transactional persistence.
type Manager struct {
	store    Store
	digester Digester
	random   io.Reader
	now      func() time.Time
}

// NewManager creates a lifecycle manager with explicit security dependencies.
func NewManager(store Store, digester Digester, randomSource io.Reader, clock func() time.Time) (*Manager, error) {
	if store == nil {
		return nil, errors.New("virtual credential store must not be nil")
	}
	if digester == nil || digester.Version() == "" {
		return nil, errors.New("virtual credential digester must not be nil")
	}
	if randomSource == nil {
		return nil, errors.New("virtual credential random source must not be nil")
	}
	if clock == nil {
		return nil, errors.New("virtual credential clock must not be nil")
	}
	return &Manager{store: store, digester: digester, random: randomSource, now: clock}, nil
}

// NewProductionManager uses the operating system CSPRNG and UTC clock.
func NewProductionManager(store Store, digester Digester) (*Manager, error) {
	return NewManager(store, digester, rand.Reader, time.Now)
}

// Create issues a new credential. Plaintext is returned only after persistence succeeds.
func (manager *Manager) Create(ctx context.Context, command CreateCommand) (IssuedCredential, error) {
	now := manager.now().UTC()
	if err := validateCreate(command, now); err != nil {
		return IssuedCredential{}, err
	}

	for range issuanceAttempts {
		material, plaintext, err := manager.issue(command.Mode)
		if err != nil {
			return IssuedCredential{}, err
		}
		record := Record{
			ID:             material.ID,
			TenantID:       strings.ToLower(command.TenantID),
			ProjectID:      strings.ToLower(command.ProjectID),
			Prefix:         material.Prefix,
			SecretHash:     material.SecretHash,
			HashKeyVersion: material.HashKeyVersion,
			Status:         StateActive,
			ExpiresAt:      cloneTime(command.ExpiresAt),
			AllowedModels:  cloneStrings(command.AllowedModels),
			Limits:         cloneLimits(command.Limits),
			Version:        1,
			CreatedAt:      now,
			CreatedBy:      command.Actor,
			UpdatedAt:      now,
			UpdatedBy:      command.Actor,
		}
		created, createErr := manager.store.Create(ctx, record)
		if createErr == nil {
			return IssuedCredential{Metadata: created.Metadata(now), Credential: plaintext}, nil
		}
		if !errors.Is(createErr, ErrCollision) {
			return IssuedCredential{}, fmt.Errorf("create virtual credential: %w", createErr)
		}
	}
	return IssuedCredential{}, fmt.Errorf("create virtual credential after %d attempts: %w", issuanceAttempts, ErrCollision)
}

// Get returns metadata only; plaintext cannot be recovered through this method.
func (manager *Manager) Get(ctx context.Context, locator Locator) (Metadata, error) {
	if err := validateLocator(locator); err != nil {
		return Metadata{}, err
	}
	record, err := manager.store.Get(ctx, normalizedLocator(locator))
	if err != nil {
		return Metadata{}, fmt.Errorf("get virtual credential: %w", err)
	}
	return record.Metadata(manager.now().UTC()), nil
}

// Rotate atomically creates one replacement and moves the source into its grace period.
func (manager *Manager) Rotate(ctx context.Context, command RotateCommand) (IssuedCredential, error) {
	if err := validateLocator(command.Locator); err != nil {
		return IssuedCredential{}, err
	}
	if err := validateActor(command.Actor); err != nil {
		return IssuedCredential{}, err
	}
	if command.GracePeriod < minimumGrace || command.GracePeriod > maximumGrace {
		return IssuedCredential{}, &ValidationError{Field: "grace_period_seconds", Reason: "must be between 1 and 86400"}
	}

	locator := normalizedLocator(command.Locator)
	source, err := manager.store.Get(ctx, locator)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("load virtual credential for rotation: %w", err)
	}
	mode, err := modeFromPrefix(source.Prefix)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("load virtual credential mode: %w", err)
	}
	now := manager.now().UTC()
	if source.ExpiresAt != nil && !now.Before(*source.ExpiresAt) {
		return IssuedCredential{}, ErrExpired
	}

	for range issuanceAttempts {
		material, plaintext, issueErr := manager.issue(mode)
		if issueErr != nil {
			return IssuedCredential{}, issueErr
		}
		rotated, rotateErr := manager.store.Rotate(
			ctx,
			locator,
			material,
			now.Add(command.GracePeriod),
			command.Actor,
			now,
		)
		if rotateErr == nil {
			return IssuedCredential{Metadata: rotated.Metadata(now), Credential: plaintext}, nil
		}
		if !errors.Is(rotateErr, ErrCollision) {
			return IssuedCredential{}, fmt.Errorf("rotate virtual credential: %w", rotateErr)
		}
	}
	return IssuedCredential{}, fmt.Errorf("rotate virtual credential after %d attempts: %w", issuanceAttempts, ErrCollision)
}

// Revoke permanently disables a credential and is idempotent.
func (manager *Manager) Revoke(ctx context.Context, command RevokeCommand) (Metadata, error) {
	if err := validateLocator(command.Locator); err != nil {
		return Metadata{}, err
	}
	if err := validateActor(command.Actor); err != nil {
		return Metadata{}, err
	}
	now := manager.now().UTC()
	record, err := manager.store.Revoke(ctx, normalizedLocator(command.Locator), command.Actor, now)
	if err != nil {
		return Metadata{}, fmt.Errorf("revoke virtual credential: %w", err)
	}
	return record.Metadata(now), nil
}

func (manager *Manager) issue(mode string) (Replacement, string, error) {
	idBytes := make([]byte, 16)
	prefixBytes := make([]byte, prefixRandomBytes)
	secret := make([]byte, secretRandomBytes)
	if _, err := io.ReadFull(manager.random, idBytes); err != nil {
		return Replacement{}, "", fmt.Errorf("generate virtual credential ID: %w", err)
	}
	if _, err := io.ReadFull(manager.random, prefixBytes); err != nil {
		return Replacement{}, "", fmt.Errorf("generate virtual credential prefix: %w", err)
	}
	if _, err := io.ReadFull(manager.random, secret); err != nil {
		return Replacement{}, "", fmt.Errorf("generate virtual credential secret: %w", err)
	}

	idBytes[6] = (idBytes[6] & 0x0f) | 0x40
	idBytes[8] = (idBytes[8] & 0x3f) | 0x80
	prefix := "agw_" + mode + "_" + strings.ToLower(prefixEncoding.EncodeToString(prefixBytes))
	plaintext := prefix + "." + base64.RawURLEncoding.EncodeToString(secret)
	material := Replacement{
		ID:             formatUUID(idBytes),
		Prefix:         prefix,
		SecretHash:     manager.digester.Digest(prefix, secret),
		HashKeyVersion: manager.digester.Version(),
	}
	clear(secret)
	return material, plaintext, nil
}

func validateCreate(command CreateCommand, now time.Time) error {
	if err := validateLocator(Locator{TenantID: command.TenantID, ProjectID: command.ProjectID, ID: "00000000-0000-4000-8000-000000000000"}); err != nil {
		return err
	}
	if command.Mode != "live" && command.Mode != "test" {
		return &ValidationError{Field: "mode", Reason: "must be live or test"}
	}
	if err := validateActor(command.Actor); err != nil {
		return err
	}
	if command.ExpiresAt != nil && command.ExpiresAt.Before(now.Add(minimumExpiryLead)) {
		return &ValidationError{Field: "expires_at", Reason: "must be at least one second in the future"}
	}
	if err := validateAllowedModels(command.AllowedModels); err != nil {
		return err
	}
	return validateLimits(command.Limits)
}

func normalizedLocator(locator Locator) Locator {
	return Locator{
		TenantID:  strings.ToLower(locator.TenantID),
		ProjectID: strings.ToLower(locator.ProjectID),
		ID:        strings.ToLower(locator.ID),
	}
}

func modeFromPrefix(prefix string) (string, error) {
	for _, mode := range []string{"live", "test"} {
		if strings.HasPrefix(prefix, "agw_"+mode+"_") {
			return mode, nil
		}
	}
	return "", errors.New("stored virtual credential prefix has no supported mode")
}

func formatUUID(value []byte) string {
	encoded := hex.EncodeToString(value)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32]
}
