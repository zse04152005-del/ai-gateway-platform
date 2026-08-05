package metering_test

import (
	"errors"
	"testing"
	"time"

	"github.com/zse04152005-del/ai-gateway-platform/internal/metering"
)

const (
	testPriceVersionID = "7b000000-0000-4000-8000-000000000001"
	testDeploymentID   = "7b000000-0000-4000-8000-000000000002"
)

func TestPriceVersionValidationAndHistoricalRateSelection(t *testing.T) {
	createdAt := time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)
	effectiveAt := createdAt.Add(-time.Hour)
	draft := validPriceVersion(createdAt, effectiveAt)
	if err := draft.Validate(); err != nil {
		t.Fatalf("draft.Validate() error = %v", err)
	}
	if _, err := draft.RateAt(metering.TokenTypeInput, createdAt); !errors.Is(err, metering.ErrPriceNotEffective) {
		t.Fatalf("draft.RateAt() error = %v, want ErrPriceNotEffective", err)
	}

	publishedAt := createdAt.Add(time.Minute)
	published := draft
	published.Status = metering.PriceStatusPublished
	published.Version = 2
	published.PublishedAt = &publishedAt
	published.UpdatedAt = publishedAt
	if err := published.Validate(); err != nil {
		t.Fatalf("published.Validate() error = %v", err)
	}
	rate, err := published.RateAt(metering.TokenTypeInput, createdAt)
	if err != nil || rate.UnitQuantity != 1_000_000 || rate.UnitPriceMicros != 2_500_000 {
		t.Fatalf("published.RateAt(input) = %+v, %v", rate, err)
	}
	if _, err := published.RateAt(metering.TokenTypeInput, effectiveAt.Add(-time.Nanosecond)); !errors.Is(err, metering.ErrPriceNotEffective) {
		t.Fatalf("pre-effective RateAt() error = %v", err)
	}
	if _, err := published.RateAt(metering.TokenTypeImageOutput, createdAt); !errors.Is(err, metering.ErrPriceRateNotFound) {
		t.Fatalf("missing RateAt() error = %v", err)
	}
}

func TestPriceVersionRejectsInvalidIdentityLifecycleAndRates(t *testing.T) {
	createdAt := time.Date(2026, time.August, 3, 10, 0, 0, 0, time.UTC)
	valid := validPriceVersion(createdAt, createdAt)
	publishedAt := createdAt.Add(time.Minute)

	invalid := []metering.PriceVersion{
		{},
		mutatePrice(valid, func(price *metering.PriceVersion) { price.ID = "bad" }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.DeploymentID = "bad" }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Region = "CN North" }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Currency = "usd" }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.EffectiveAt = time.Time{} }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.CreatedBy = " actor " }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.UpdatedAt = createdAt.Add(-time.Second) }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Status = "retired" }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Version = 2 }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Rates = nil }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Rates[0].BillingUnit = metering.BillingUnitImage }),
		mutatePrice(valid, func(price *metering.PriceVersion) { price.Rates = append(price.Rates, price.Rates[0]) }),
		mutatePrice(valid, func(price *metering.PriceVersion) {
			price.Status, price.Version, price.PublishedAt = metering.PriceStatusPublished, 2, &publishedAt
			price.UpdatedAt = createdAt
		}),
	}
	for index, price := range invalid {
		if err := price.Validate(); !errors.Is(err, metering.ErrInvalidPriceVersion) {
			t.Fatalf("invalid[%d].Validate() error = %v", index, err)
		}
		if _, err := price.RateAt(metering.TokenTypeInput, createdAt); !errors.Is(err, metering.ErrInvalidPriceVersion) {
			t.Fatalf("invalid[%d].RateAt() error = %v", index, err)
		}
	}
}

func TestPriceRateUnitAndExactIntegerBoundaries(t *testing.T) {
	valid := []metering.PriceRate{
		{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 1, UnitPriceMicros: 0},
		{TokenType: metering.TokenTypeAudioInput, BillingUnit: metering.BillingUnitSecond, UnitQuantity: 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeAudioOutput, BillingUnit: metering.BillingUnitToken, UnitQuantity: metering.MaximumExactInteger, UnitPriceMicros: metering.MaximumExactInteger},
		{TokenType: metering.TokenTypeImageInput, BillingUnit: metering.BillingUnitImage, UnitQuantity: 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeImageOutput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 1, UnitPriceMicros: 1},
	}
	for index, rate := range valid {
		if err := rate.Validate(); err != nil {
			t.Fatalf("valid[%d].Validate() error = %v", index, err)
		}
	}
	invalid := []metering.PriceRate{
		{},
		{TokenType: "vendor", BillingUnit: metering.BillingUnitToken, UnitQuantity: 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitImage, UnitQuantity: 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeAudioInput, BillingUnit: metering.BillingUnitImage, UnitQuantity: 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeImageInput, BillingUnit: metering.BillingUnitSecond, UnitQuantity: 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 0, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken, UnitQuantity: metering.MaximumExactInteger + 1, UnitPriceMicros: 1},
		{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 1, UnitPriceMicros: -1},
		{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 1, UnitPriceMicros: metering.MaximumExactInteger + 1},
	}
	for index, rate := range invalid {
		if err := rate.Validate(); !errors.Is(err, metering.ErrInvalidPriceVersion) {
			t.Fatalf("invalid[%d].Validate() error = %v", index, err)
		}
	}
}

func TestCalculateAmountMicrosRoundsUpAndRejectsOverflow(t *testing.T) {
	rate := metering.PriceRate{
		TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken,
		UnitQuantity: 1_000_000, UnitPriceMicros: 2_500_000,
	}
	for quantity, want := range map[int64]int64{1: 3, 2: 5, 13: 33, 1_000_000: 2_500_000} {
		got, err := metering.CalculateAmountMicros(quantity, rate)
		if err != nil || got != want {
			t.Errorf("CalculateAmountMicros(%d) = %d, %v; want %d", quantity, got, err, want)
		}
	}
	free := rate
	free.UnitPriceMicros = 0
	if got, err := metering.CalculateAmountMicros(1, free); err != nil || got != 0 {
		t.Fatalf("CalculateAmountMicros(free) = %d, %v", got, err)
	}
	if _, err := metering.CalculateAmountMicros(0, rate); !errors.Is(err, metering.ErrInvalidPriceVersion) {
		t.Fatalf("CalculateAmountMicros(zero) error = %v", err)
	}
	overflow := rate
	overflow.UnitQuantity = 1
	overflow.UnitPriceMicros = metering.MaximumExactInteger
	if _, err := metering.CalculateAmountMicros(metering.MaximumExactInteger, overflow); !errors.Is(err, metering.ErrAmountOverflow) {
		t.Fatalf("CalculateAmountMicros(overflow) error = %v", err)
	}
}

func validPriceVersion(createdAt, effectiveAt time.Time) metering.PriceVersion {
	return metering.PriceVersion{
		ID: testPriceVersionID, DeploymentID: testDeploymentID, Region: "cn-north-1", Currency: "USD",
		EffectiveAt: effectiveAt, Status: metering.PriceStatusDraft,
		Rates: []metering.PriceRate{
			{TokenType: metering.TokenTypeInput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 1_000_000, UnitPriceMicros: 2_500_000},
			{TokenType: metering.TokenTypeOutput, BillingUnit: metering.BillingUnitToken, UnitQuantity: 1_000_000, UnitPriceMicros: 7_500_000},
		},
		Version: 1, CreatedAt: createdAt, CreatedBy: "integration:price",
		UpdatedAt: createdAt, UpdatedBy: "integration:price",
	}
}

func mutatePrice(input metering.PriceVersion, mutate func(*metering.PriceVersion)) metering.PriceVersion {
	result := input
	result.Rates = append([]metering.PriceRate(nil), input.Rates...)
	mutate(&result)
	return result
}
