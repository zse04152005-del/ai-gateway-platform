package metering

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// MaximumExactInteger is the largest integer that survives JSON number exchange exactly.
const MaximumExactInteger int64 = 9_007_199_254_740_991

var (
	// ErrInvalidPriceVersion means a version or one of its rates violates the pricing contract.
	ErrInvalidPriceVersion = errors.New("metering price version is invalid")
	// ErrPriceNotEffective means a version is unpublished or starts after the observed fact.
	ErrPriceNotEffective = errors.New("metering price version is not effective")
	// ErrPriceRateNotFound means the version has no rate for the requested token type.
	ErrPriceRateNotFound = errors.New("metering price rate is not found")
	// ErrAmountOverflow means the exact rounded charge cannot fit the ledger integer contract.
	ErrAmountOverflow = errors.New("metering calculated amount exceeds the exact integer limit")

	priceUUIDPattern     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	priceRegionPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	priceCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

// PriceStatus identifies the one-way lifecycle of a price publication.
type PriceStatus string

// Supported PriceStatus values allow authoring followed by one immutable publication.
const (
	PriceStatusDraft     PriceStatus = "draft"
	PriceStatusPublished PriceStatus = "published"
)

// BillingUnit identifies the quantity measured by one price rate.
type BillingUnit string

// Supported BillingUnit values cover textual, audio, and image pricing.
const (
	BillingUnitToken  BillingUnit = "token"
	BillingUnitImage  BillingUnit = "image"
	BillingUnitSecond BillingUnit = "second"
)

// PriceRate prices one independently metered token type in exact integer micros.
type PriceRate struct {
	TokenType       TokenType
	BillingUnit     BillingUnit
	UnitQuantity    int64
	UnitPriceMicros int64
}

// CalculateAmountMicros prices one positive usage fact with integer ceiling
// rounding. A positive non-zero rate therefore never silently becomes free.
func CalculateAmountMicros(quantity int64, rate PriceRate) (int64, error) {
	if quantity < 1 || quantity > MaximumExactInteger || rate.Validate() != nil {
		return 0, ErrInvalidPriceVersion
	}
	if rate.UnitPriceMicros == 0 {
		return 0, nil
	}
	numerator := new(big.Int).Mul(big.NewInt(quantity), big.NewInt(rate.UnitPriceMicros))
	amount, remainder := new(big.Int), new(big.Int)
	amount.QuoRem(numerator, big.NewInt(rate.UnitQuantity), remainder)
	if remainder.Sign() != 0 {
		amount.Add(amount, big.NewInt(1))
	}
	if !amount.IsInt64() || amount.Int64() > MaximumExactInteger {
		return 0, ErrAmountOverflow
	}
	return amount.Int64(), nil
}

// Validate rejects rates that cannot be represented safely or whose unit is
// incompatible with the independently billed token type.
func (rate PriceRate) Validate() error {
	unitCompatible := validBillingUnit(rate.TokenType, rate.BillingUnit)
	if !rate.TokenType.Valid() || rate.UnitQuantity < 1 || rate.UnitQuantity > MaximumExactInteger ||
		rate.UnitPriceMicros < 0 || rate.UnitPriceMicros > MaximumExactInteger ||
		!unitCompatible {
		return ErrInvalidPriceVersion
	}
	return nil
}

// PriceVersion is one deployment, region, currency, and effective-time price snapshot.
type PriceVersion struct {
	ID           string
	DeploymentID string
	Region       string
	Currency     string
	EffectiveAt  time.Time
	Status       PriceStatus
	Rates        []PriceRate
	Version      int64
	CreatedAt    time.Time
	CreatedBy    string
	UpdatedAt    time.Time
	UpdatedBy    string
	PublishedAt  *time.Time
}

// Validate checks a complete draft or immutable published price snapshot.
func (price PriceVersion) Validate() error {
	if !priceUUIDPattern.MatchString(price.ID) || !priceUUIDPattern.MatchString(price.DeploymentID) ||
		!priceRegionPattern.MatchString(price.Region) || !priceCurrencyPattern.MatchString(price.Currency) ||
		price.EffectiveAt.IsZero() || price.CreatedAt.IsZero() || price.UpdatedAt.Before(price.CreatedAt) ||
		!validPriceActor(price.CreatedBy) || !validPriceActor(price.UpdatedBy) ||
		len(price.Rates) < 1 || len(price.Rates) > len(TokenTypes()) {
		return ErrInvalidPriceVersion
	}
	switch price.Status {
	case PriceStatusDraft:
		if price.Version != 1 || price.PublishedAt != nil {
			return ErrInvalidPriceVersion
		}
	case PriceStatusPublished:
		if price.Version != 2 || price.PublishedAt == nil || price.PublishedAt.Before(price.CreatedAt) ||
			price.UpdatedAt.Before(*price.PublishedAt) {
			return ErrInvalidPriceVersion
		}
	default:
		return ErrInvalidPriceVersion
	}
	seen := make(map[TokenType]struct{}, len(price.Rates))
	for _, rate := range price.Rates {
		if rate.Validate() != nil {
			return ErrInvalidPriceVersion
		}
		if _, duplicate := seen[rate.TokenType]; duplicate {
			return ErrInvalidPriceVersion
		}
		seen[rate.TokenType] = struct{}{}
	}
	return nil
}

// RateAt returns the locked rate only when the published version was effective
// at the fact's observation time.
func (price PriceVersion) RateAt(tokenType TokenType, observedAt time.Time) (PriceRate, error) {
	if price.Validate() != nil {
		return PriceRate{}, ErrInvalidPriceVersion
	}
	if price.Status != PriceStatusPublished || observedAt.IsZero() || observedAt.Before(price.EffectiveAt) {
		return PriceRate{}, ErrPriceNotEffective
	}
	for _, rate := range price.Rates {
		if rate.TokenType == tokenType {
			return rate, nil
		}
	}
	return PriceRate{}, ErrPriceRateNotFound
}

func validBillingUnit(tokenType TokenType, unit BillingUnit) bool {
	switch tokenType {
	case TokenTypeInput, TokenTypeOutput, TokenTypeCacheRead, TokenTypeCacheWrite, TokenTypeReasoning:
		return unit == BillingUnitToken
	case TokenTypeAudioInput, TokenTypeAudioOutput:
		return unit == BillingUnitToken || unit == BillingUnitSecond
	case TokenTypeImageInput, TokenTypeImageOutput:
		return unit == BillingUnitToken || unit == BillingUnitImage
	default:
		return false
	}
}

func validPriceActor(actor string) bool {
	return len(actor) >= 1 && len(actor) <= 200 && actor == strings.TrimSpace(actor)
}
