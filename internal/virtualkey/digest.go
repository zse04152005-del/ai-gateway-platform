package virtualkey

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
)

const digestSize = sha256.Size

var digestVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Digester produces the non-reversible database identity for a credential secret.
type Digester interface {
	Version() string
	Digest(prefix string, secret []byte) []byte
}

// HMACDigester uses a versioned server-side key and SHA-256.
type HMACDigester struct {
	version string
	key     []byte
}

// NewHMACDigester validates and copies the digest key.
func NewHMACDigester(version string, key []byte) (*HMACDigester, error) {
	version = strings.TrimSpace(version)
	if !digestVersionPattern.MatchString(version) {
		return nil, errors.New("virtual credential digest version is invalid")
	}
	if len(key) != digestSize {
		return nil, errors.New("virtual credential digest key must be exactly 32 bytes")
	}
	return &HMACDigester{version: version, key: append([]byte(nil), key...)}, nil
}

// Version returns the non-secret digest key identifier.
func (d *HMACDigester) Version() string {
	if d == nil {
		return ""
	}
	return d.version
}

// Digest binds the secret to its public prefix and returns a 32-byte keyed digest.
func (d *HMACDigester) Digest(prefix string, secret []byte) []byte {
	if d == nil {
		return nil
	}
	mac := hmac.New(sha256.New, d.key)
	_, _ = mac.Write([]byte(prefix))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(secret)
	return mac.Sum(nil)
}
