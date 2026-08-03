package metering_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/zse04152005-del/ai-gateway-platform/internal/adapter"
	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

func TestTokenTypeTaxonomyIsFiniteStrictAndCopySafe(t *testing.T) {
	want := []metering.TokenType{
		metering.TokenTypeInput,
		metering.TokenTypeOutput,
		metering.TokenTypeCacheRead,
		metering.TokenTypeCacheWrite,
		metering.TokenTypeReasoning,
		metering.TokenTypeAudioInput,
		metering.TokenTypeAudioOutput,
		metering.TokenTypeImageInput,
		metering.TokenTypeImageOutput,
	}
	got := metering.TokenTypes()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TokenTypes() = %v, want %v", got, want)
	}
	for _, tokenType := range got {
		parsed, err := metering.ParseTokenType(string(tokenType))
		if err != nil || parsed != tokenType || !parsed.Valid() {
			t.Fatalf("ParseTokenType(%q) = %q, %v", tokenType, parsed, err)
		}
	}
	got[0] = "mutated"
	if metering.TokenTypes()[0] != metering.TokenTypeInput {
		t.Fatal("TokenTypes() exposed mutable package state")
	}

	for _, value := range []string{"", "Input", " input", "input ", "total", "audio", "image", "vendor_meter"} {
		parsed, err := metering.ParseTokenType(value)
		if parsed != "" || !errors.Is(err, metering.ErrInvalidTokenType) || metering.TokenType(value).Valid() {
			t.Fatalf("ParseTokenType(%q) = %q, %v; want invalid", value, parsed, err)
		}
	}
}

func TestSourcesReuseNormalizedAdapterTaxonomy(t *testing.T) {
	want := []metering.Source{
		adapter.UsageSourceProvider,
		adapter.UsageSourceEstimated,
		adapter.UsageSourceReconciled,
		adapter.UsageSourceAdjustment,
	}
	got := metering.Sources()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Sources() = %v, want %v", got, want)
	}
	for _, source := range got {
		parsed, err := metering.ParseSource(string(source))
		if err != nil || parsed != source || !metering.ValidSource(parsed) {
			t.Fatalf("ParseSource(%q) = %q, %v", source, parsed, err)
		}
	}
	got[0] = "mutated"
	if metering.Sources()[0] != metering.SourceProvider {
		t.Fatal("Sources() exposed mutable package state")
	}

	for _, value := range []string{"", "Provider", " provider", "provider ", "vendor", "inferred", "billing"} {
		parsed, err := metering.ParseSource(value)
		if parsed != "" || !errors.Is(err, metering.ErrInvalidSource) || metering.ValidSource(metering.Source(value)) {
			t.Fatalf("ParseSource(%q) = %q, %v; want invalid", value, parsed, err)
		}
	}
}
