package adapter_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
)

func TestUsageEvidencePreservesExactUnknownFieldsImmutably(t *testing.T) {
	t.Parallel()

	raw := []byte(" {\n  \"input_tokens\": 7, \"new_billable_unit\": 3, \"nested\": {\"rate\": \"premium\"}\n}")
	original := append([]byte(nil), raw...)
	evidence, err := adapter.NewUsageEvidence(raw)
	if err != nil {
		t.Fatalf("new usage evidence: %v", err)
	}
	raw[3] = 'X'
	returned := evidence.Bytes()
	returned[3] = 'Y'

	if string(evidence.Bytes()) != string(original) {
		t.Fatalf("evidence changed: %q", evidence.Bytes())
	}
	wantDigest := sha256.Sum256(original)
	if evidence.Hash() != hex.EncodeToString(wantDigest[:]) {
		t.Fatalf("hash = %q, want %q", evidence.Hash(), hex.EncodeToString(wantDigest[:]))
	}
	if evidence.Size() != len(original) || !evidence.Present() {
		t.Fatalf("unexpected evidence metadata: size=%d present=%v", evidence.Size(), evidence.Present())
	}
	if !strings.Contains(string(evidence.Bytes()), "new_billable_unit") {
		t.Fatal("unknown usage field was not retained")
	}
}

func TestUsageEvidenceSerializationIsMetadataOnly(t *testing.T) {
	t.Parallel()

	evidence, err := adapter.NewUsageEvidence([]byte(`{"secret_unknown_value":"must-not-serialize"}`))
	if err != nil {
		t.Fatalf("new usage evidence: %v", err)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	if strings.Contains(string(encoded), "must-not-serialize") || strings.Contains(string(encoded), "secret_unknown_value") {
		t.Fatalf("serialized evidence leaked raw JSON: %s", encoded)
	}
	if !strings.Contains(string(encoded), evidence.Hash()) || !strings.Contains(string(encoded), `"bytes"`) {
		t.Fatalf("serialized evidence missing integrity metadata: %s", encoded)
	}
	zero, err := json.Marshal(adapter.UsageEvidence{})
	if err != nil {
		t.Fatalf("marshal zero evidence: %v", err)
	}
	if string(zero) != "null" {
		t.Fatalf("zero evidence = %s, want null", zero)
	}
	if strings.Contains(evidence.String(), "must-not-serialize") || !strings.Contains(evidence.String(), evidence.Hash()) {
		t.Fatalf("unsafe evidence String value: %s", evidence.String())
	}
	if (adapter.UsageEvidence{}).String() != "UsageEvidence<empty>" {
		t.Fatal("unexpected empty evidence String value")
	}

	var logOutput bytes.Buffer
	slog.New(slog.NewJSONHandler(&logOutput, nil)).Info("evidence", slog.Any("evidence", evidence))
	if strings.Contains(logOutput.String(), "must-not-serialize") || strings.Contains(logOutput.String(), "secret_unknown_value") {
		t.Fatalf("direct evidence log leaked raw JSON: %s", logOutput.String())
	}
	if !strings.Contains(logOutput.String(), evidence.Hash()) {
		t.Fatalf("direct evidence log missing digest: %s", logOutput.String())
	}
}

func TestUsageEvidenceRejectsInvalidOrUnboundedValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"array", []byte(`[]`)},
		{"scalar", []byte(`1`)},
		{"malformed", []byte(`{"a":`)},
		{"trailing", []byte(`{"a":1} {"b":2}`)},
		{"too large", []byte(`{"value":"` + strings.Repeat("x", 64*1024) + `"}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := adapter.NewUsageEvidence(test.raw); err == nil {
				t.Fatal("expected evidence validation error")
			}
		})
	}
}

func TestNormalizedUsageValidation(t *testing.T) {
	t.Parallel()

	evidence, err := adapter.NewUsageEvidence([]byte(`{"input_tokens":0,"future_meter":7}`))
	if err != nil {
		t.Fatalf("new usage evidence: %v", err)
	}
	valid := adapter.NormalizedUsage{
		InputTokens:    adapter.Tokens(0),
		OutputTokens:   adapter.Tokens(2),
		Source:         adapter.UsageSourceProvider,
		Complete:       true,
		RawEvidence:    evidence,
		UnmappedFields: []string{"/future_meter"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("validate provider usage: %v", err)
	}
	if !valid.InputTokens.Present || valid.InputTokens.Value != 0 {
		t.Fatal("reported zero was not distinct from a missing count")
	}
	if valid.CacheReadTokens.Present {
		t.Fatal("missing cache count unexpectedly became present")
	}
	if valid.RawEvidenceHash() != evidence.Hash() {
		t.Fatal("usage evidence hash mismatch")
	}

	adjustment := adapter.NormalizedUsage{
		InputTokens: adapter.Tokens(-3),
		Source:      adapter.UsageSourceAdjustment,
		Complete:    true,
	}
	if err := adjustment.Validate(); err != nil {
		t.Fatalf("negative adjustment should be valid: %v", err)
	}
}

func TestNormalizedUsageValidationFailures(t *testing.T) {
	t.Parallel()

	evidence, err := adapter.NewUsageEvidence([]byte(`{"input_tokens":1,"z":2}`))
	if err != nil {
		t.Fatalf("new usage evidence: %v", err)
	}
	tests := []struct {
		name      string
		usage     adapter.NormalizedUsage
		wantField string
	}{
		{"source", adapter.NormalizedUsage{Source: "vendor"}, "usage.source"},
		{"missing value", adapter.NormalizedUsage{InputTokens: adapter.TokenCount{Value: 1}, Source: adapter.UsageSourceEstimated}, "usage.input_tokens"},
		{"negative provider", adapter.NormalizedUsage{InputTokens: adapter.Tokens(-1), Source: adapter.UsageSourceProvider, RawEvidence: evidence}, "usage.input_tokens"},
		{"provider evidence", adapter.NormalizedUsage{InputTokens: adapter.Tokens(1), Source: adapter.UsageSourceProvider}, "usage.raw_evidence"},
		{"unknown evidence", adapter.NormalizedUsage{Source: adapter.UsageSourceEstimated, UnmappedFields: []string{"/future"}}, "usage.raw_evidence"},
		{"pointer syntax", adapter.NormalizedUsage{Source: adapter.UsageSourceEstimated, RawEvidence: evidence, UnmappedFields: []string{"future"}}, "usage.unmapped_fields[0]"},
		{"pointer order", adapter.NormalizedUsage{Source: adapter.UsageSourceEstimated, RawEvidence: evidence, UnmappedFields: []string{"/z", "/a"}}, "usage.unmapped_fields"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assertValidationField(t, test.usage.Validate(), test.wantField)
		})
	}
}
