package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxUsageEvidenceBytes = 64 * 1024

// UsageEvidence is an immutable, bounded copy of the exact provider usage JSON.
// MarshalJSON emits only integrity metadata so raw evidence cannot accidentally
// enter an API response, log, or event through ordinary serialization.
type UsageEvidence struct {
	raw    []byte
	digest [sha256.Size]byte
}

// NewUsageEvidence verifies and copies one top-level JSON object without
// canonicalizing it. The digest therefore identifies the exact observed bytes.
func NewUsageEvidence(raw []byte) (UsageEvidence, error) {
	if len(raw) == 0 {
		return UsageEvidence{}, errors.New("usage evidence must not be empty")
	}
	if len(raw) > maxUsageEvidenceBytes {
		return UsageEvidence{}, fmt.Errorf("usage evidence exceeds %d bytes", maxUsageEvidenceBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return UsageEvidence{}, fmt.Errorf("decode usage evidence: %w", err)
	}
	if object == nil {
		return UsageEvidence{}, errors.New("usage evidence must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return UsageEvidence{}, errors.New("usage evidence must contain exactly one JSON value")
	}
	copyOfRaw := append([]byte(nil), raw...)
	return UsageEvidence{raw: copyOfRaw, digest: sha256.Sum256(copyOfRaw)}, nil
}

// Present reports whether evidence was captured.
func (evidence UsageEvidence) Present() bool {
	return len(evidence.raw) > 0
}

// Bytes returns a defensive copy for explicit persistence or reconciliation.
func (evidence UsageEvidence) Bytes() []byte {
	return append([]byte(nil), evidence.raw...)
}

// Size returns the exact captured byte count.
func (evidence UsageEvidence) Size() int {
	return len(evidence.raw)
}

// Hash returns the lowercase SHA-256 digest of the exact captured bytes.
func (evidence UsageEvidence) Hash() string {
	if !evidence.Present() {
		return ""
	}
	return hex.EncodeToString(evidence.digest[:])
}

// String returns integrity metadata and never formats the captured JSON bytes.
func (evidence UsageEvidence) String() string {
	if !evidence.Present() {
		return "UsageEvidence<empty>"
	}
	return fmt.Sprintf("UsageEvidence<sha256=%s bytes=%d>", evidence.Hash(), evidence.Size())
}

// MarshalJSON emits non-sensitive integrity metadata, never the provider JSON.
func (evidence UsageEvidence) MarshalJSON() ([]byte, error) {
	if !evidence.Present() {
		return []byte("null"), nil
	}
	return json.Marshal(struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	}{
		SHA256: evidence.Hash(),
		Bytes:  evidence.Size(),
	})
}

// RawEvidenceHash returns the immutable provider evidence digest.
func (usage NormalizedUsage) RawEvidenceHash() string {
	return usage.RawEvidence.Hash()
}
