package keyauth

import (
	"crypto/hmac"
	"errors"
	"fmt"

	"github.com/zse04152005-del/ai-gateway-platform/internal/virtualkey"
)

// Keyring verifies records created by the current or explicitly retained digest versions.
type Keyring struct {
	currentVersion string
	digesters      map[string]*virtualkey.HMACDigester
}

// NewKeyring copies every key through HMACDigester and rejects duplicate versions.
func NewKeyring(currentVersion string, currentKey []byte, retained map[string][]byte) (*Keyring, error) {
	current, err := virtualkey.NewHMACDigester(currentVersion, currentKey)
	if err != nil {
		return nil, fmt.Errorf("create current authentication digester: %w", err)
	}
	digesters := make(map[string]*virtualkey.HMACDigester, len(retained)+1)
	digesters[currentVersion] = current
	for version, key := range retained {
		if version == currentVersion {
			return nil, errors.New("retained authentication digest version duplicates current version")
		}
		digester, err := virtualkey.NewHMACDigester(version, key)
		if err != nil {
			return nil, fmt.Errorf("create retained authentication digester: %w", err)
		}
		digesters[version] = digester
	}
	return &Keyring{currentVersion: currentVersion, digesters: digesters}, nil
}

// Verify returns (matched, versionKnown) and always uses constant-time digest comparison.
func (keyring *Keyring) Verify(version, prefix string, secret, expected []byte) (bool, bool) {
	if keyring == nil {
		return false, false
	}
	digester, ok := keyring.digesters[version]
	if !ok {
		return false, false
	}
	actual := digester.Digest(prefix, secret)
	return hmac.Equal(actual, expected), true
}

// Burn performs equivalent HMAC work for an unknown prefix without exposing lookup existence through CPU timing.
func (keyring *Keyring) Burn(prefix string, secret []byte) {
	if keyring == nil {
		return
	}
	digester := keyring.digesters[keyring.currentVersion]
	if digester != nil {
		_ = digester.Digest(prefix, secret)
	}
}
